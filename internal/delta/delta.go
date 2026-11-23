// delta.go holds AdminPrefix.

package delta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AdminPrefix starts every path the delta answers itself.
const AdminPrefix = "/_delta/"

// Delta fans requests out to the truth and every mirror and reports how each
// mirror answer differs from the truth.
type Delta struct {
	cfg    *Config
	client *http.Client
	rep    *Reporter
	log    *slog.Logger
	wg     sync.WaitGroup
}

// New builds a Delta. cfg must have passed Validate.
func New(cfg *Config, rep *Reporter, log *slog.Logger) *Delta {
	return &Delta{
		cfg: cfg,
		rep: rep,
		log: log,
		client: &http.Client{
			// A redirect is an answer to compare, never a hop to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Inbound is one request as the client sent it.
type Inbound struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// hopHeaders are per-connection and never forwarded. Accept-Encoding is
// dropped so each answer arrives decoded and its bytes can be compared.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Accept-Encoding", "Content-Length", "Host",
}

func (d *Delta) fetch(ctx context.Context, t *Target, in *Inbound) *Response {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.cfg.Timeout))
	defer cancel()
	u := t.base + in.Path
	if in.Query != "" {
		u += "?" + in.Query
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, u, bytes.NewReader(in.Body))
	if err != nil {
		return &Response{Err: err, Elapsed: time.Since(start)}
	}
	req.Header = in.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return &Response{Err: err, Elapsed: time.Since(start)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Response{Err: fmt.Errorf("read body: %w", err), Elapsed: time.Since(start)}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body, Elapsed: time.Since(start)}
}

// start sends in to every target named, each on its own goroutine. The
// context is detached from the client, so a client that hangs up does not
// cut a comparison short.
func (d *Delta) start(ctx context.Context, in *Inbound, names []string) map[string]<-chan *Response {
	ctx = context.WithoutCancel(ctx)
	out := make(map[string]<-chan *Response, len(names))
	for _, n := range names {
		ch := make(chan *Response, 1)
		out[n] = ch
		t := d.cfg.target(n)
		go func() { ch <- d.fetch(ctx, t, in) }()
	}
	return out
}

func (d *Delta) allNames() []string {
	names := []string{TruthName}
	for _, m := range d.cfg.Mirrors {
		names = append(names, m.Name)
	}
	return names
}

// Compare sends in to every target, waits for all of them, and reports one
// record per mirror.
func (d *Delta) Compare(ctx context.Context, in *Inbound) []Record {
	chans := d.start(ctx, in, d.allNames())
	return d.finish(in, chans)
}

func (d *Delta) finish(in *Inbound, chans map[string]<-chan *Response) []Record {
	truth := <-chans[TruthName]
	recs := make([]Record, 0, len(d.cfg.Mirrors))
	for _, m := range d.cfg.Mirrors {
		ans := <-chans[m.Name]
		rec := d.record(in, &m, truth, ans)
		d.rep.Add(rec)
		recs = append(recs, rec)
	}
	return recs
}

func (d *Delta) record(in *Inbound, m *Target, truth, ans *Response) Record {
	rec := Record{
		Time:   time.Now().UTC(),
		Method: in.Method,
		Path:   in.Path,
		Query:  in.Query,
		Mirror: m.Name,
		Truth:  side(truth, nil),
		Answer: side(ans, d.cfg.Annotate),
	}
	diffs := compareResponses(truth, ans, compareOpts{
		headers:    d.cfg.Headers,
		head:       in.Method == http.MethodHead,
		truthBase:  d.cfg.Truth.base,
		mirrorBase: m.base,
	})
	for _, df := range diffs {
		if i := d.cfg.ruleFor(in.Method, in.Path, m.Name, df); i >= 0 {
			df.Rule = &i
			rec.Ignored = append(rec.Ignored, df)
			continue
		}
		rec.Diffs = append(rec.Diffs, df)
	}
	return rec
}

func side(r *Response, annotate []string) Side {
	s := Side{Status: r.Status, Millis: r.Elapsed.Milliseconds()}
	if r.Err != nil {
		s.Error = r.Err.Error()
	}
	for _, h := range annotate {
		if v := r.Header.Get(h); v != "" {
			if s.Annotations == nil {
				s.Annotations = map[string]string{}
			}
			s.Annotations[http.CanonicalHeaderKey(h)] = v
		}
	}
	return s
}

// ServeHTTP answers the client from the served target. When the request is
// one to compare, the comparison finishes after the client has its answer.
func (d *Delta) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, AdminPrefix) {
		d.admin(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	in := &Inbound{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Header: r.Header, Body: body}

	names := []string{d.cfg.Serve}
	compare := d.cfg.compares(in.Method, in.Path)
	if compare {
		names = d.allNames()
	} else {
		d.rep.Uncompared()
	}
	d.wg.Add(1)
	chans := d.start(r.Context(), in, names)
	served := <-chans[d.cfg.Serve]
	// finish reads the served answer again, so it goes back in a fresh channel.
	replay := make(chan *Response, 1)
	replay <- served
	chans[d.cfg.Serve] = replay
	go func() {
		defer d.wg.Done()
		if compare {
			d.finish(in, chans)
		}
	}()
	writeAnswer(w, d.cfg.Serve, served)
}

func writeAnswer(w http.ResponseWriter, from string, r *Response) {
	w.Header().Set("X-Delta-Served", from)
	if r.Err != nil {
		http.Error(w, fmt.Sprintf("api-mirror-delta: %s did not answer: %v", from, r.Err), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		if isHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(r.Body)))
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}

func isHop(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(h, k) {
			return true
		}
	}
	return strings.EqualFold(k, "Content-Encoding")
}

// Drain waits for every comparison in flight, up to timeout.
func (d *Delta) Drain(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("drain: comparisons still in flight after %s; their records are lost", timeout)
	}
}

func (d *Delta) admin(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, AdminPrefix) {
	case "summary":
		s := d.rep.Summary()
		if r.URL.Query().Get("format") == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_ = s.WriteText(w)
			return
		}
		writeJSON(w, s)
	case "recent":
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				http.Error(w, "limit: want a positive integer", http.StatusBadRequest)
				return
			}
			limit = n
		}
		writeJSON(w, d.rep.Recent(limit))
	default:
		http.Error(w, "api-mirror-delta: unknown path; try "+AdminPrefix+"summary or "+AdminPrefix+"recent", http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	_ = enc.Encode(v)
}
