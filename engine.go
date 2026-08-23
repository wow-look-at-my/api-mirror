package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Engine serves a spec: it answers declared routes from the store, forwards
// everything else, and keeps the store current.
type Engine struct {
	spec    *Spec
	store   *Store
	up      *Upstreamer
	fresh   *Fresh
	reveal  *Revealer
	ingest  http.Handler
	reorder *Reorderer
	proxy   *httputil.ReverseProxy
	vars    map[string]any
	baseURL *url.URL
}

// forwardKey carries the caller's own headers into a detached fetch.
//
// A fetch triggered by one caller runs on a context that outlives their
// request, so the headers cannot be read back off the request later. They ride
// the context instead, and context.WithoutCancel keeps them.
type forwardKey struct{}

func withForward(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, forwardKey{}, h)
}

func forwardFrom(ctx context.Context) http.Header {
	h, _ := ctx.Value(forwardKey{}).(http.Header)
	if h == nil {
		return http.Header{}
	}
	return h
}

// NewEngine wires a spec into a server.
func NewEngine(spec *Spec, store *Store, observe func(Exchange)) (*Engine, error) {
	vars, err := resolveVars(spec)
	if err != nil {
		return nil, err
	}
	up, err := NewUpstreamer(spec, vars, observe)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(up.base)
	if err != nil {
		return nil, fmt.Errorf("upstream base %q: %w", up.base, err)
	}
	e := &Engine{
		spec:    spec,
		store:   store,
		up:      up,
		reveal:  NewRevealer(store, up, vars),
		vars:    vars,
		baseURL: base,
	}
	e.fresh = NewFresh(store, e.fetch, e.ttlFor)
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			// The upstream does not need the client's address, and adding it
			// would leak one consumer's network position to another's provider.
		},
		ModifyResponse: stripUpstreamCORS,
	}
	return e, nil
}

// resolveVars renders the spec's vars once, in declaration order, each seeing
// the ones before it.
func resolveVars(spec *Spec) (map[string]any, error) {
	vars := map[string]any{}
	ctx := map[string]any{"env": envMap(), "var": vars}
	for _, v := range spec.Vars {
		s, err := renderString(v.Value, ctx)
		if err != nil {
			return nil, fmt.Errorf("var %q: %w", v.Name, err)
		}
		vars[v.Name] = s
	}
	return vars, nil
}

// SetIngest installs the webhook handler and the reorderer behind it, so a
// shutdown can wait for deliveries the upstream will never send again.
func (e *Engine) SetIngest(h http.Handler, reorder *Reorderer) {
	e.ingest = h
	e.reorder = reorder
}

// ttlFor resolves a kind's TTL. A kind is a route's identity, so a route may
// hold an answer for longer or shorter than its resource's default.
func (e *Engine) ttlFor(kind string) time.Duration {
	for _, rt := range e.spec.Routes {
		if routeKind(rt) != kind {
			continue
		}
		if rt.TTL > 0 {
			return rt.TTL
		}
		if res, ok := e.store.Resource(rt.Resource); ok && res.TTL > 0 {
			return res.TTL
		}
	}
	return defaultTTL
}

// defaultTTL backs a route that names no TTL anywhere.
const defaultTTL = time.Hour

// routeKind names one route's freshness bookkeeping. A list and a single read
// of the same resource are different questions with different answers, so they
// are different kinds.
func routeKind(rt *Route) string {
	if rt.List {
		return rt.Resource + ":list"
	}
	return rt.Resource
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e.ingest != nil && e.spec.Events != nil && r.URL.Path == e.spec.Events.Path {
		e.ingest.ServeHTTP(w, r)
		return
	}
	m, reason, err := e.resolve(r)
	if m == nil {
		if err != nil {
			logf("passthrough %s %s (%s): %v", r.Method, r.URL.Path, reason, err)
		}
		e.passthrough(w, r, reason)
		return
	}
	e.serve(w, r, m)
}

// serve answers one declared route.
func (e *Engine) serve(w http.ResponseWriter, r *http.Request, m *match) {
	ctx := withForward(r.Context(), r.Header)
	res, ok := e.store.Resource(m.route.Resource)
	if !ok {
		e.passthrough(w, r, PassUnrouted)
		return
	}

	principal := principalOf(r)
	verdict, err := e.reveal.Allow(ctx, principal, res, m.key, r.Header)
	if err != nil {
		// A transient failure proving access is not a refusal. Refusing would
		// teach a consumer they lost access they still have.
		http.Error(w, "cannot establish access: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !verdict.Allowed {
		http.Error(w, http.StatusText(verdict.Status), verdict.Status)
		return
	}

	kind := routeKind(m.route)
	key := keyString(res, m.key)
	ctx = withPlan(ctx, &fetchPlan{
		route: m.route,
		res:   res,
		key:   m.key,
		query: m.query,
		path:  r.URL.EscapedPath(),
	})
	outcome, err := e.fresh.Ensure(ctx, kind, key)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}

	doc, err := e.read(ctx, m, res)
	if err != nil {
		http.Error(w, "read cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if doc == nil {
		// Nothing stored and nothing fetched: the upstream says this does not
		// exist. That is an answer, and it is the upstream's answer.
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	e.write(w, http.StatusOK, doc, outcome)
}

// read rebuilds the answer for one request from what is stored.
//
// Hit and miss both come through here, so a route's shape cannot change with
// cache state. A consumer that works against a warm cache and breaks against a
// cold one is the bug this shape prevents.
func (e *Engine) read(ctx context.Context, m *match, res *Resource) (any, error) {
	if m.route.List {
		rows, err := e.store.List(ctx, res, m.key)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			doc, err := rebuildRow(res, row)
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
		}
		return out, nil
	}
	row, err := e.store.Get(ctx, res, m.key)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return rebuildRow(res, row)
}

// rebuildRow renders one stored row as the document a consumer receives.
func rebuildRow(res *Resource, row Row) (any, error) {
	if res.Store == StoreDocument {
		s, _ := row["document"].(string)
		return decodeJSON([]byte(s))
	}
	return rebuild(res, row)
}

// write sends a rebuilt answer, saying plainly where it came from.
func (e *Engine) write(w http.ResponseWriter, status int, doc any, outcome Outcome) {
	body, err := marshalJSON(doc)
	if err != nil {
		http.Error(w, "render answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Mirror-Cache", string(outcome))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		logf("write answer: %v", err)
	}
}

// passthrough forwards a request the spec does not model, and says why.
//
// The reason is the point. A passthrough is unfinished work, not a settled
// state, and an operator needs to know which routes are still leaving.
func (e *Engine) passthrough(w http.ResponseWriter, r *http.Request, reason PassReason) {
	w.Header().Set("X-Mirror-Cache", "passthrough")
	w.Header().Set("X-Mirror-Passthrough-Reason", string(reason))
	e.proxy.ServeHTTP(w, r)
}

// stripUpstreamCORS removes the upstream's CORS headers from a forwarded
// answer. The mirror is the origin a browser talks to, and two
// Access-Control-Allow-Origin values make a browser reject the response
// outright.
func stripUpstreamCORS(resp *http.Response) error {
	for h := range resp.Header {
		if strings.HasPrefix(strings.ToLower(h), "access-control-allow-") {
			resp.Header.Del(h)
		}
	}
	return nil
}

// principalOf identifies who is asking.
//
// The value gates only what the mirror REVEALS, never what it stores. It is a
// fingerprint of the caller's credential, so two callers with different access
// never share a proof.
func principalOf(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	return "token:" + fingerprint(auth)
}
