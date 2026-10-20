package mirror

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type PassReason string

const (
	PassUnrouted   PassReason = "unrouted"        // no route declares this path
	PassMethod     PassReason = "unrouted-method" // the path is declared, this method is not
	PassAccept     PassReason = "unmodeled-accept"
	PassQuery      PassReason = "unmodeled-query"
	PassResponse   PassReason = "unmodeled-response" // the route models the request, not what came back
	PassNoIdentity PassReason = "unverified-identity"
	PassRelay      PassReason = "relayed"     // a declared forward to a foreign host
	PassBypass     PassReason = "bypass-body" // the body is a write the route declares uncacheable
	// PassUnobservedBudget is a budget question from a caller the meter has not seen yet.
	PassUnobservedBudget PassReason = "unobserved-budget"
)

// match is request resolved against a route.
type match struct {
	route *Route
	// key is the resource key this request addresses.
	key map[string]string
	// query is the modelled query, defaults filled in.
	query map[string]string
	// body is read only for a route naming a body-key, and replayed upstream.
	body []byte
}

// resolve finds the route that answers a request.
//
// A path no route declares is a passthrough, and so is a declared path asked
// with a method no route declares. Both are reported, because a route that
// still forwards is unfinished work rather than a settled state.
func (e *Engine) resolve(r *http.Request) (*match, PassReason, error) {
	pathKnown, refused := false, false
	for _, rt := range e.spec.Routes {
		params, ok := matchPath(rt.Path, r.URL.EscapedPath())
		if !ok || !rt.admits(params) {
			continue
		}
		pathKnown = true
		if rt.Method != r.Method {
			continue
		}
		res, ok := e.store.Resource(rt.Resource)
		if !ok {
			return nil, PassUnrouted, fmt.Errorf("route %s names unknown resource %q", rt.Path, rt.Resource)
		}
		// A later route on the same path may answer another media type.
		if !acceptable(e.mediaFor(rt, res), r.Header.Get("Accept")) {
			refused = true
			continue
		}
		query, err := modelQuery(rt, r)
		if err != nil {
			return nil, PassQuery, err
		}
		key := make(map[string]string, len(params))
		for param, value := range params {
			name := rt.column(param)
			key[name] = foldFor(res, name, value)
		}
		for _, q := range rt.Query {
			if !q.Key {
				continue
			}
			name := rt.column(q.Name)
			key[name] = foldFor(res, name, query[q.Name])
		}
		for _, k := range res.Keys {
			if !k.Credential {
				continue
			}
			v := credentialValue(k, r)
			if v == "" {
				// No credential to key this row by; nothing to serve.
				return nil, PassNoIdentity, nil
			}
			key[k.Name] = v
		}
		// The body IS the key here, and a consumed body cannot be re-read, so the match carries it for the fetch to replay.
		var body []byte
		if rt.BodyKey != "" {
			b, overflow, err := readCapped(r.Body)
			if err != nil {
				return nil, PassQuery, fmt.Errorf("route %s: reading the request body: %w", rt.Path, err)
			}
			if overflow {
				return nil, PassQuery, fmt.Errorf("route %s: the request body is over the %d byte cap", rt.Path, maxBodyBytes)
			}
			body = b
			// The passthrough needs the body the resolve consumed.
			r.Body = io.NopCloser(bytes.NewReader(b))
			if rt.Bypass != nil && rt.Bypass.Match(b) {
				return nil, PassBypass, nil
			}
			key[rt.BodyKey] = bodyFingerprint(b)
		}
		return &match{route: rt, key: key, query: query, body: body}, "", nil
	}
	if refused {
		return nil, PassAccept, nil
	}
	if pathKnown {
		return nil, PassMethod, nil
	}
	return nil, PassUnrouted, nil
}

// bodyFingerprint keys a request body. A JSON body is re-marshalled so the
// same question with its fields in another order finds the same row.
func bodyFingerprint(b []byte) string {
	doc, err := decodeJSON(b)
	if err != nil {
		return fingerprint(string(b))
	}
	canon, err := marshalJSON(doc)
	if err != nil {
		return fingerprint(string(b))
	}
	return fingerprint(string(canon))
}

// foldFor applies a key component's declared case folding, so a differently
// cased URL cannot land on its own row that a delivery never reaches.
func foldFor(res *Resource, name, value string) string {
	for _, k := range res.Keys {
		if k.Name == name {
			return foldKey(k, value)
		}
	}
	return value
}

// pathParam reads one pattern segment. {name} binds one segment. {name...}
// binds the rest of the path, slashes included, and only ends a pattern.
func pathParam(seg string) (name string, param, rest bool) {
	if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' {
		return "", false, false
	}
	name = seg[1 : len(seg)-1]
	if trimmed, ok := strings.CutSuffix(name, "..."); ok {
		return trimmed, true, true
	}
	// A rest parameter may also be spelled {name*}, the other spelling the
	// spec accepts for the same thing.
	if trimmed, ok := strings.CutSuffix(name, "*"); ok {
		return trimmed, true, true
	}
	return name, true, false
}

// matchPath matches a request path against a route pattern, returning the
// {name} bindings. A final {name*} binds every remaining segment, slashes
// included, because a file path or a ref like heads/feature/x is a single value.
func matchPath(pattern, path string) (map[string]string, bool) {
	pseg := strings.Split(strings.Trim(pattern, "/"), "/")
	rseg := strings.Split(strings.Trim(path, "/"), "/")
	_, _, rest := pathParam(pseg[len(pseg)-1])
	if len(pseg) != len(rseg) && !(rest && len(rseg) > len(pseg)) {
		return nil, false
	}
	out := make(map[string]string, len(pseg))
	for i, p := range pseg {
		if name, param, tail := pathParam(p); param {
			v := rseg[i]
			if tail {
				if slices.Contains(rseg[i:], "") {
					return nil, false
				}
				v = strings.Join(rseg[i:], "/")
			}
			if v == "" {
				return nil, false
			}
			out[name] = v
			continue
		}
		if p != rseg[i] {
			return nil, false
		}
	}
	return out, true
}

// mediaFor lists the media types a route answers. A JSON route also answers
// every type the spec names as the default representation.
func (e *Engine) mediaFor(rt *Route, res *Resource) []string {
	if !res.Store.answersJSON() {
		return rt.Accept
	}
	return slices.Concat(rt.Accept, e.spec.Accept)
}

// acceptable reports whether a route that answers these media types can
// answer what the caller asked for. An empty list answers anything.
//
// A caller asking for a media type the route cannot rebuild is forwarded
// rather than answered with the wrong one.
func acceptable(allowed []string, accept string) bool {
	if len(allowed) == 0 || strings.TrimSpace(accept) == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		media, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			return false
		}
		if media == "*/*" || media == "application/*" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(media, a) }) {
			return false
		}
	}
	return true
}

// modelQuery checks a request's query against the shape the route declares and
// fills in the declared defaults.
//
// A parameter the route does not model, a repeated parameter, or a value
// outside a declared range makes the request a passthrough. Clamping instead
// would answer a question the caller did not ask, from a row keyed on a shape
// the spec never described.
func modelQuery(rt *Route, r *http.Request) (map[string]string, error) {
	declared := make(map[string]QueryParam, len(rt.Query))
	for _, q := range rt.Query {
		declared[q.Name] = q
	}
	out := make(map[string]string, len(rt.Query))
	for _, q := range rt.Query {
		out[q.Name] = q.Default
	}
	for name, values := range r.URL.Query() {
		q, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("query parameter %q is not modelled by route %s", name, rt.Path)
		}
		if len(values) > 1 {
			return nil, fmt.Errorf("query parameter %q repeated", name)
		}
		v := values[0]
		if err := checkQueryValue(q, v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

func checkQueryValue(q QueryParam, v string) error {
	switch q.Type {
	case FieldInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("query parameter %q: %q is not a number", q.Name, v)
		}
		if q.Max > 0 && (n < q.Min || n > q.Max) {
			return fmt.Errorf("query parameter %q: %d is outside %d..%d", q.Name, n, q.Min, q.Max)
		}
	case FieldBool:
		if v != "true" && v != "false" {
			return fmt.Errorf("query parameter %q: %q is not a bool", q.Name, v)
		}
	}
	return nil
}
