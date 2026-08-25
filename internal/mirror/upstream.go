package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBodyBytes caps what the mirror will read from an upstream answer. A body
const maxBodyBytes = 8 << 20

// upstreamClient is a package var so a test can point it at an httptest server.
var upstreamClient = &http.Client{Timeout: 60 * time.Second}

// Upstreamer sends requests to the mirrored API.
type Upstreamer struct {
	spec    *Spec
	base    string
	headers []Header
	client  *http.Client
	observe func(Exchange)
}

// Exchange is one completed upstream request, as reported to the observer.
type Exchange struct {
	Method   string
	URL      string
	Status   int
	Bytes    int
	Duration time.Duration
	Err      error
}

// NewUpstreamer resolves the upstream's base URL and static headers once.
func NewUpstreamer(spec *Spec, vars map[string]any, observe func(Exchange)) (*Upstreamer, error) {
	base, err := renderString(spec.Upstream.Base, vars)
	if err != nil {
		return nil, fmt.Errorf("upstream base: %w", err)
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	// A template over a value the spec never set renders to something that is
	if u, err := url.Parse(base); base == "" || err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream base resolved to nothing usable: %q is not an absolute URL", base)
	}
	if observe == nil {
		observe = func(Exchange) {}
	}
	return &Upstreamer{
		spec:    spec,
		base:    base,
		headers: spec.Upstream.Headers,
		client:  upstreamClient,
		observe: observe,
	}, nil
}

// Answer is what the upstream said.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
	// Overflow reports that the body exceeded maxBodyBytes. The body is then
	Overflow bool
}

// Call sends one request to the upstream, carrying the caller's own forwarded
// headers so the answer is the one THAT caller is entitled to.
//
// A non-2xx is a real answer, not an error: a 404 is what the upstream knows,
// and the route decides whether that is worth storing. Only a transport failure
// returns an error.
func (u *Upstreamer) Call(ctx context.Context, method, path string, vars map[string]any, forward http.Header) (*Answer, error) {
	url := u.base + path
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	for _, h := range u.headers {
		name, err := renderString(h.Name, vars)
		if err != nil {
			return nil, fmt.Errorf("upstream header name: %w", err)
		}
		value, err := renderString(h.Value, vars)
		if err != nil {
			return nil, fmt.Errorf("upstream header %s: %w", name, err)
		}
		if name == "" || value == "" {
			continue
		}
		req.Header.Set(name, value)
	}
	for _, name := range u.spec.Upstream.Forward {
		if v := forward.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	// A buffered body must be plain bytes: the mirror parses and rebuilds it,
	req.Header.Set("Accept-Encoding", "identity")

	started := time.Now()
	resp, err := u.client.Do(req)
	if err != nil {
		u.observe(Exchange{Method: method, URL: url, Duration: time.Since(started), Err: err})
		return nil, fmt.Errorf("upstream %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, overflow, err := readCapped(resp.Body)
	if err != nil {
		u.observe(Exchange{Method: method, URL: url, Status: resp.StatusCode, Duration: time.Since(started), Err: err})
		return nil, fmt.Errorf("read upstream %s %s: %w", method, path, err)
	}
	u.observe(Exchange{
		Method:   method,
		URL:      url,
		Status:   resp.StatusCode,
		Bytes:    len(body),
		Duration: time.Since(started),
	})
	return &Answer{Status: resp.StatusCode, Header: resp.Header, Body: body, Overflow: overflow}, nil
}

// readCapped reads at most maxBodyBytes and reports whether more was waiting.
func readCapped(r io.Reader) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxBodyBytes {
		return b[:maxBodyBytes], true, nil
	}
	return b, false, nil
}

// RateLimited reports whether an answer is the upstream refusing for rate
// reasons rather than for access reasons.
func RateLimited(a *Answer) bool {
	if a == nil {
		return false
	}
	if a.Status == http.StatusTooManyRequests {
		return true
	}
	if a.Header.Get("Retry-After") != "" {
		return true
	}
	return a.Header.Get("X-RateLimit-Remaining") == "0"
}

// Transient reports whether an answer is one that must never be stored: a
func Transient(a *Answer) bool {
	if a == nil {
		return true
	}
	return a.Status >= 500 || RateLimited(a)
}
