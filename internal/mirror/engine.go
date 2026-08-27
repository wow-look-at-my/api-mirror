package mirror

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
	spec     *Spec
	store    *Store
	up       *Upstreamer
	fresh    *Fresh
	reveal   *Revealer
	ingest   *Ingest
	proxy    *httputil.ReverseProxy
	vars     map[string]any
	baseURL  *url.URL
	tel      *Telemetry
	admin    *Admin
	refresh  *Refresher
	notify   *Notifier
	replay   *Replayer
	debounce *Debouncer
	vocab    []string
}

// forwardKey carries the caller's own headers into a detached fetch.
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
//
// Telemetry is built here rather than passed in when the caller has none: an
// operator who has to discover and enable the only view of what the mirror is
// doing has, in practice, no view of what the mirror is doing.
func NewEngine(spec *Spec, store *Store, tel *Telemetry) (*Engine, error) {
	vars, err := resolveVars(spec)
	if err != nil {
		return nil, err
	}
	if tel == nil {
		tel = NewTelemetry(spec.Upstream.Rate)
	}
	up, err := NewUpstreamer(spec, vars, tel)
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
		tel:     tel,
	}
	e.fresh = NewFresh(store, e.fetch, e.ttlFor)
	e.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			// The upstream does not need the client's address, and adding it
		},
		ModifyResponse: stripUpstreamCORS,
		// The forwarded path is the one an instrumented call site would have
		// missed, so it reports from the same transport as every other client.
		Transport: observing(http.DefaultTransport, LanePassthrough, tel),
	}
	e.vocab = pathVocabulary(spec)
	e.debounce = NewDebouncer(spec.Upstream.Debounce)
	e.admin = NewAdmin(e)
	if spec.Notify != nil {
		e.notify, err = NewNotifier(spec, vars, tel, store.path)
		if err != nil {
			return nil, err
		}
	}
	e.refresh = NewRefresher(e)
	e.replay = NewReplayer(e)
	return e, nil
}

// Telemetry exposes what the mirror knows about its own traffic.
func (e *Engine) Telemetry() *Telemetry { return e.tel }

// resolveVars renders the spec's vars once, in declaration order, each seeing
// the ones before it.
//
// It returns the whole template context, not just the vars, because that
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
	return ctx, nil
}

// SetIngest installs the webhook handler. The Engine keeps it whole rather than
func (e *Engine) SetIngest(i *Ingest) {
	e.ingest = i
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
func routeKind(rt *Route) string {
	if rt.List {
		return rt.Resource + ":list"
	}
	return rt.Resource
}

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := newRecorder(w, r)
	defer func() { e.tel.Requests.Record(rec.entry()) }()
	e.dispatch(rec, r)
}

// dispatch picks who answers one request. Every branch ends by telling the
// recorder what it did, because a disposition the log cannot name is traffic
// nobody can account for.
func (e *Engine) dispatch(rec *recorder, r *http.Request) {
	if e.spec.CORS != nil && e.answerPreflight(rec, r) {
		return
	}
	if e.health(rec, r) {
		return
	}
	if e.admin != nil && e.admin.Handles(r.URL.Path) {
		rec.note(DispAdmin, e.spec.Dashboard.Path, "", "")
		e.admin.ServeHTTP(rec, r)
		return
	}
	if e.ingest != nil && e.spec.Events != nil && r.URL.Path == e.spec.Events.Path {
		rec.note(DispDelivery, e.spec.Events.Path, "", "")
		e.ingest.ServeHTTP(rec, r)
		return
	}
	if p, params, ok := e.matchPurge(r); ok {
		rec.note(DispMiss, p.Path, p.Resource, "purge")
		e.forwardAndPurge(rec, r, p, params)
		return
	}
	m, reason, err := e.resolve(r)
	if m == nil {
		if err != nil {
			logf("passthrough %s %s (%s): %v", r.Method, r.URL.Path, reason, err)
		}
		e.passthrough(rec, r, reason)
		return
	}
	rec.shape = m.route.Path
	rec.resource = m.route.Resource
	e.serve(rec, r, m)
}

// matchPurge finds the declared write, if any, this request names.
func (e *Engine) matchPurge(r *http.Request) (*Purge, map[string]string, bool) {
	for _, p := range e.spec.Purges {
		if p.Method != r.Method {
			continue
		}
		params, ok := matchPath(p.Path, r.URL.EscapedPath())
		if !ok {
			continue
		}
		return p, params, true
	}
	return nil, nil, false
}

// forwardAndPurge forwards a write verbatim, then drops the row it changed
// once the upstream confirms the write actually happened. A write is never
// cached itself; this only clears what it made stale.
func (e *Engine) forwardAndPurge(w *recorder, r *http.Request, p *Purge, params map[string]string) {
	res, ok := e.store.Resource(p.Resource)
	if !ok {
		e.passthrough(w, r, PassUnrouted)
		return
	}
	key := make(map[string]string, len(params)+1)
	for param, value := range params {
		key[param] = foldFor(res, param, value)
	}
	for _, k := range res.Keys {
		if !k.Credential {
			continue
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			e.passthrough(w, r, PassNoIdentity)
			return
		}
		key[k.Name] = fingerprint(auth)
	}

	w.Header().Set("X-Mirror-Cache", "purge")
	e.proxy.ServeHTTP(w, r.WithContext(withLane(r.Context(), LanePassthrough, w.principal, "purge")))

	// The recorder already holds what the caller actually received, so the
	// purge decides on that rather than on what the write was expected to do.
	if w.status < 200 || w.status >= 300 {
		return
	}
	if _, err := e.store.Delete(r.Context(), res, key); err != nil {
		logf("purge %s: %v", p.Path, err)
	}
}

// serve answers one declared route.
func (e *Engine) serve(w *recorder, r *http.Request, m *match) {
	ctx := withForward(r.Context(), r.Header)
	res, ok := e.store.Resource(m.route.Resource)
	if !ok {
		e.passthrough(w, r, PassUnrouted)
		return
	}

	principal := principalOf(r)
	w.principal = principal
	ctx = withLane(ctx, LaneFetch, principal, m.route.Resource)
	verdict, err := e.reveal.Allow(ctx, principal, res, m.key, r.Header)
	if err != nil {
		// A transient failure proving access is not a refusal. Refusing would
		w.note(DispError, m.route.Path, m.route.Resource, "reveal")
		http.Error(w, "cannot establish access: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !verdict.Allowed {
		w.note(DispDenied, m.route.Path, m.route.Resource, string(verdict.Reason))
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
		w.note(DispError, "", "", "upstream")
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.note(dispositionOf(outcome), "", "", "")
	refusal := e.rememberedRefusal(ctx, kind, key)
	if outcome == OutcomeMiss && refusal == 0 {
		// The fetch went out with this caller's own credential and the upstream
		e.reveal.RenewOn2xx(ctx, principal, res, m.key, http.StatusOK)
	}
	if status := refusal; status != 0 {
		// The upstream stated this refusal and the route declared it worth
		w.note(DispRefusal, "", "", "absorbed")
		w.Header().Set("X-Mirror-Cache", string(outcome))
		http.Error(w, http.StatusText(status), status)
		return
	}

	doc, err := e.read(ctx, m, res)
	if err != nil {
		w.note(DispError, "", "", "read-cache")
		http.Error(w, "read cache: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if doc == nil {
		// Nothing stored and nothing fetched: the upstream says this does not
		w.note(DispRefusal, "", "", "empty")
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	e.write(w, http.StatusOK, doc, outcome)
}

// dispositionOf maps a freshness outcome onto the log's vocabulary.
func dispositionOf(o Outcome) Disposition {
	switch o {
	case OutcomeHit:
		return DispHit
	case OutcomeMiss:
		return DispMiss
	default:
		return DispError
	}
}

// rememberedRefusal reports the stored non-success status for a key, or zero.
//
// A read error here reports no refusal: the freshness row is bookkeeping, and
// failing to read it must not turn a servable answer into one.
func (e *Engine) rememberedRefusal(ctx context.Context, kind, key string) int {
	meta, err := e.store.Freshness(ctx, kind, key)
	if err != nil {
		logf("read freshness %s/%s: %v", kind, key, err)
		return 0
	}
	if meta == nil || meta.Status < 400 {
		return 0
	}
	return meta.Status
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
func (e *Engine) passthrough(w *recorder, r *http.Request, reason PassReason) {
	w.note(DispPassthrough, e.shapeOf(r), "", string(reason))
	w.Header().Set("X-Mirror-Cache", "passthrough")
	w.Header().Set("X-Mirror-Passthrough-Reason", string(reason))

	r = r.WithContext(withLane(r.Context(), LanePassthrough, w.principal, string(reason)))
	if e.debounce == nil {
		e.proxy.ServeHTTP(w, r)
		return
	}
	// An identical read in flight is one the upstream is already answering.
	// Sharing it costs a caller a little latency and saves the budget a second
	// identical question would have spent.
	e.debounce.Share(w, r, e.proxy.ServeHTTP)
}

// shapeOf reduces a passthrough path to the shape an operator can act on.
//
// A raw path per caller turns the uncached table into a list of one-offs. The
// shape is what says "this family of requests is still leaving", which is the
// thing somebody has to model next.
func (e *Engine) shapeOf(r *http.Request) string {
	for _, rt := range e.spec.Routes {
		if _, ok := matchPath(rt.Path, r.URL.EscapedPath()); ok {
			return rt.Path
		}
	}
	return generalizeWith(e.vocab, r.URL.EscapedPath())
}

// stripUpstreamCORS removes the upstream's CORS headers from a forwarded
// answer. The mirror is the origin a browser talks to, and two
func stripUpstreamCORS(resp *http.Response) error {
	for h := range resp.Header {
		if strings.HasPrefix(strings.ToLower(h), "access-control-allow-") {
			resp.Header.Del(h)
		}
	}
	return nil
}

// principalOf identifies who is asking.
func principalOf(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	return "token:" + fingerprint(auth)
}
