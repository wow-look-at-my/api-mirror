package mirror

import (
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// PassReason says why a request was forwarded instead of answered from the
type PassReason string

const (
	PassUnrouted   PassReason = "unrouted"        // no route declares this path
	PassMethod     PassReason = "unrouted-method" // the path is declared, this method is not
	PassAccept     PassReason = "unmodeled-accept"
	PassQuery      PassReason = "unmodeled-query"
	PassResponse   PassReason = "unmodeled-response" // the route models the request, not what came back
	PassNoIdentity PassReason = "unverified-identity"
)

// match is request resolved against a route.
type match struct {
	route *Route
	// key is the resource key this request addresses, path parameters and
	key map[string]string
	// query is the modelled query, defaults filled in.
	query map[string]string
}

// resolve finds the route that answers a request.
//
// A path no route declares is a passthrough, and so is a declared path asked
// with a method no route declares. Both are reported, because a route that
// still forwards is unfinished work rather than a settled state.
func (e *Engine) resolve(r *http.Request) (*match, PassReason, error) {
	pathKnown := false
	for _, rt := range e.spec.Routes {
		params, ok := matchPath(rt.Path, r.URL.EscapedPath())
		if !ok {
			continue
		}
		pathKnown = true
		if rt.Method != r.Method {
			continue
		}
		if !acceptable(rt, r.Header.Get("Accept")) {
			return nil, PassAccept, nil
		}
		query, err := modelQuery(rt, r)
		if err != nil {
			return nil, PassQuery, err
		}
		key := make(map[string]string, len(params))
		res, ok := e.store.Resource(rt.Resource)
		if !ok {
			return nil, PassUnrouted, fmt.Errorf("route %s names unknown resource %q", rt.Path, rt.Resource)
		}
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
			auth := r.Header.Get("Authorization")
			if auth == "" {
				// No credential to key this row by; nothing to serve.
				return nil, PassNoIdentity, nil
			}
			key[k.Name] = fingerprint(auth)
		}
		return &match{route: rt, key: key, query: query}, "", nil
	}
	if pathKnown {
		return nil, PassMethod, nil
	}
	return nil, PassUnrouted, nil
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

// matchPath matches a request path against a route pattern, returning the
// {name} bindings.
func matchPath(pattern, path string) (map[string]string, bool) {
	pseg := strings.Split(strings.Trim(pattern, "/"), "/")
	rseg := strings.Split(strings.Trim(path, "/"), "/")
	if len(pseg) != len(rseg) {
		return nil, false
	}
	out := make(map[string]string, len(pseg))
	for i, p := range pseg {
		if len(p) > 2 && p[0] == '{' && p[len(p)-1] == '}' {
			v := rseg[i]
			if v == "" {
				return nil, false
			}
			out[p[1:len(p)-1]] = v
			continue
		}
		if p != rseg[i] {
			return nil, false
		}
	}
	return out, true
}

// acceptable reports whether this route can answer what the caller asked for.
//
// A route rebuilds JSON from stored columns. A caller asking for a diff, a
// patch or HTML is asking for something the mirror cannot rebuild, so it is
// forwarded rather than answered with the wrong media type.
func acceptable(rt *Route, accept string) bool {
	if len(rt.Accept) == 0 || strings.TrimSpace(accept) == "" {
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
		found := false
		for _, allowed := range rt.Accept {
			if strings.EqualFold(media, allowed) {
				found = true
				break
			}
		}
		if !found {
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
