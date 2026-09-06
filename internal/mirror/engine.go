package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
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
	vocab    set.Set[string]
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
		// The path an instrumented call site would have missed entirely.
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

// resolveVars renders the spec's vars, in declaration order, each seeing
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

// routeKind names route's freshness bookkeeping. A list and a single read
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

// dispatch picks who answers request. Every branch ends by telling the
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
	if purges, params, ok := e.matchPurges(r); ok {
		rec.note(DispMiss, purges[0].Path, purges[0].Resource, "purge")
		e.forwardAndPurge(rec, r, purges, params)
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

// matchPurges finds EVERY declared write this request names. A write can
// invalidate several answers: creating a webhook changes the hook's own row
// and the listing it now appears in. Matching only until the earliest hit
// leaves the rest serving what the write made stale.
func (e *Engine) matchPurges(r *http.Request) ([]*Purge, map[string]string, bool) {
	var found []*Purge
	var params map[string]string
	for _, p := range e.spec.Purges {
		if p.Method != r.Method {
			continue
		}
		got, ok := matchPath(p.Path, r.URL.EscapedPath())
		if !ok {
			continue
		}
		if params == nil {
			params = got
		}
		found = append(found, p)
	}
	return found, params, len(found) > 0
}

// forwardAndPurge forwards a write verbatim, then drops the rows it changed
// after the upstream confirms the write actually happened. A write is never
// cached itself. This only clears what it made stale.
func (e *Engine) forwardAndPurge(w *recorder, r *http.Request, purges []*Purge, params map[string]string) {
	keys := make([]map[string]string, 0, len(purges))
	resources := make([]*Resource, 0, len(purges))
	for _, p := range purges {
		res, ok := e.store.Resource(p.Resource)
		if !ok {
			e.passthrough(w, r, PassUnrouted)
			return
		}
		key, ok := purgeKey(res, params, r.Header.Get("Authorization"))
		if !ok {
			e.passthrough(w, r, PassNoIdentity)
			return
		}
		resources = append(resources, res)
		keys = append(keys, key)
	}

	w.Header().Set("X-Mirror-Cache", "purge")
	e.proxy.ServeHTTP(w, r.WithContext(withLane(r.Context(), LanePassthrough, w.principal, "purge")))

	// The recorder already holds what the caller actually received, so the
	// purge decides on that rather than on what the write was expected to do.
	if w.status < 200 || w.status >= 300 {
		return
	}
	for i, res := range resources {
		if _, err := e.store.Delete(r.Context(), res, keys[i]); err != nil {
			logf("purge %s: %v", purges[i].Path, err)
		}
	}
}

// purgeKey builds the row key a write addresses. A path parameter the
// resource does not key on is dropped, so a write on the single item can
// still name the listing it belongs to, which keys on fewer columns.
func purgeKey(res *Resource, params map[string]string, auth string) (map[string]string, bool) {
	key := make(map[string]string, len(res.Keys))
	for _, k := range res.Keys {
		if k.Credential {
			if auth == "" {
				return nil, false
			}
			key[k.Name] = fingerprint(auth)
			continue
		}
		if value, ok := params[k.Name]; ok {
			key[k.Name] = foldFor(res, k.Name, value)
		}
	}
	return key, true
}

// serve answers declared route.
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

// rememberedRefusal reports the stored non-success status for a key, or.
//
// A read error here reports no refusal: the freshness row is bookkeeping, and
// failing to read it must not turn a servable answer into.
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

// read rebuilds the answer for request from what is stored.
//
// Hit and miss both come through here, so a route's shape cannot change with
// cache state. A consumer that works against a warm cache and breaks against a
// cold is the bug this shape prevents.
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

// rebuildRow renders stored row as the document a consumer receives.
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
	// A read already in flight is the upstream is already answering.
	e.debounce.Share(w, r, e.proxy.ServeHTTP)
}

// shapeOf reduces a passthrough path to something an operator can act on. A
// raw path per caller makes the uncached table a list of -offs; a shape
// says "this family is still leaving", which names what to model next.
func (e *Engine) shapeOf(r *http.Request) string {
	for _, rt := range e.spec.Routes {
		if _, ok := matchPath(rt.Path, r.URL.EscapedPath()); ok {
			return rt.Path
		}
	}
	return generalizeWith(e.vocab, r.URL.EscapedPath())
}

// stripUpstreamCORS removes the upstream's CORS headers from a forwarded
// answer. The mirror is the origin a browser talks to, and
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
