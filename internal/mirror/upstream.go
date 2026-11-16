// upstream.go holds maxBodyBytes.

package mirror

import (
	"bytes"
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
	observe Observer
	creds   *credentials
}

// NewUpstreamer resolves the upstream's base URL and static headers.
func NewUpstreamer(spec *Spec, vars map[string]any, observe Observer) (*Upstreamer, error) {
	base, err := renderString(spec.Upstream.Base, vars)
	if err != nil {
		return nil, fmt.Errorf("upstream base: %w", err)
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if u, err := url.Parse(base); base == "" || err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream base resolved to nothing usable: %q is not an absolute URL", base)
	}
	if observe == nil {
		observe = funcObserver(func(Exchange) {})
	}
	u := &Upstreamer{
		spec:    spec,
		base:    base,
		headers: spec.Upstream.Headers,
		// Reports from its transport: no call site can forget to.
		client:  observedClient(upstreamClient, LaneFetch, observe),
		observe: observe,
	}
	u.creds = newCredentials(u, spec, vars)
	follow := u.client.CheckRedirect
	u.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Context().Value(noFollowKey{}) != nil {
			return http.ErrUseLastResponse
		}
		if follow != nil {
			return follow(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after %d redirects", len(via))
		}
		return nil
	}
	return u, nil
}

// noFollowKey marks a call whose redirect is itself the answer.
type noFollowKey struct{}

func withoutRedirects(ctx context.Context) context.Context {
	return context.WithValue(ctx, noFollowKey{}, true)
}

// Answer is what the upstream said.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
	// Overflow reports that the body exceeded maxBodyBytes.
	Overflow bool
}

// Call sends a request with the caller's forwarded headers. A nil forward
// is background work on the static headers.
func (u *Upstreamer) Call(ctx context.Context, method, path string, vars map[string]any, forward http.Header, reqBody []byte) (*Answer, error) {
	return u.CallWith(ctx, method, path, vars, forward, nil, reqBody)
}

// CallWith is Call plus headers the engine sets last. A non-2xx is an
// answer, not an error. For a caller, a forwarded header it did not send
// is removed, so the static background credential never answers for it.
func (u *Upstreamer) CallWith(ctx context.Context, method, path string, vars map[string]any, forward, set http.Header, reqBody []byte) (*Answer, error) {
	url := u.base + path
	var send io.Reader
	if len(reqBody) > 0 {
		send = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, send)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.ContentLength = int64(len(reqBody))
	for _, h := range u.headers {
		name, err := renderString(h.Name, vars)
		if err != nil {
			return nil, fmt.Errorf("upstream header name: %w", err)
		}
		if _, ok := set[http.CanonicalHeaderKey(name)]; ok {
			// The engine sets this last, so its static value would never leave.
			continue
		}
		hvars := vars
		if h.Credential != "" {
			// A caller is never answered with the background credential,
			// whatever <forward> lists.
			if forward != nil || inExchange(ctx) {
				continue
			}
			token, on, err := u.creds.token(ctx, h.Credential, keyOf(ctx))
			if err != nil {
				return nil, fmt.Errorf("upstream %s %s: %w", method, path, err)
			}
			if !on {
				continue
			}
			hvars = withCredential(vars, token)
		}
		value, err := renderString(h.Value, hvars)
		if err != nil {
			return nil, fmt.Errorf("upstream header %s: %w", name, err)
		}
		if name == "" || value == "" {
			continue
		}
		req.Header.Set(name, value)
	}
	if forward != nil {
		for _, name := range u.spec.Upstream.Forward {
			if v := forward.Get(name); v != "" {
				req.Header.Set(name, v)
			} else {
				req.Header.Del(name)
			}
		}
	}
	for name, values := range set {
		req.Header[name] = values
	}
	// A buffered body must be plain bytes: the mirror parses and rebuilds it,
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream %s %s: %w", method, path, err)
	}
	// Close reports the exchange, so this defer is the report, not hygiene.
	defer resp.Body.Close()

	body, overflow, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream %s %s: %w", method, path, err)
	}
	return &Answer{Status: resp.StatusCode, Header: resp.Header, Body: body, Overflow: overflow}, nil
}

// keyOf is the resource key a background call is about, for a credential's
// for=. A call with no plan has an empty key.
func keyOf(ctx context.Context) map[string]string {
	if p := planFrom(ctx); p != nil && p.key != nil {
		return p.key
	}
	return map[string]string{}
}

// withCredential adds the token to a copy of vars as `.credential`.
func withCredential(vars map[string]any, token string) map[string]any {
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	out["credential"] = token
	return out
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
//
// The header names come from the spec: which header says "wait" and which says
// "you may not" is the upstream's vocabulary, and getting it wrong stores. An
// outage as a denial for the whole deny window.
func (u *Upstreamer) RateLimited(a *Answer) bool {
	if a == nil {
		return false
	}
	if a.Status == http.StatusTooManyRequests {
		return true
	}
	retryAfter := u.spec.Upstream.RetryAfter
	if retryAfter == "" {
		retryAfter = "Retry-After"
	}
	if a.Header.Get(retryAfter) != "" {
		return true
	}
	if name := u.spec.Upstream.Rate.Remaining; name != "" {
		return a.Header.Get(name) == "0"
	}
	return false
}

// Transient reports whether an answer is that must never be stored.
func (u *Upstreamer) Transient(a *Answer) bool {
	if a == nil {
		return true
	}
	return a.Status >= 500 || u.RateLimited(a)
}
