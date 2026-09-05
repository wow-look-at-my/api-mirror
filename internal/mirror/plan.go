package mirror

import (
	"net/url"
	"strings"
)

// Rebuilding a request from a stored key.
//
// The freshness row records a kind and a key string, not a URL. That is
// deliberate -- a stored URL would be a spelling of the same fact, free
// to drift from the route that produced it. The sweep therefore reverses the
// key back through the route, which means the route stays the only place a
// path is described.

// planFor rebuilds the fetch plan for stored key of kind.
//
// It reports false rather than guessing when the key cannot be turned back into
// a request. A sweep that fetched the wrong path would write a real answer into
// the wrong row, which is worse than not sweeping at all.
func (e *Engine) planFor(kind string, sk StaleKey) (*fetchPlan, bool) {
	rt := e.routeForKind(kind)
	if rt == nil {
		return nil, false
	}
	res, ok := e.store.Resource(rt.Resource)
	if !ok {
		return nil, false
	}
	key, ok := parseKeyString(res, sk.Key)
	if !ok {
		return nil, false
	}
	for _, k := range res.Keys {
		if k.Credential {
			// The sweep would file its own answer under a caller's key.
			return nil, false
		}
	}
	path, ok := rt.fillPath(key)
	if !ok {
		return nil, false
	}
	query := map[string]string{}
	for _, q := range rt.Query {
		query[q.Name] = q.Default
		if q.Key {
			if v, ok := key[rt.column(q.Name)]; ok {
				query[q.Name] = v
			}
		}
	}
	return &fetchPlan{route: rt, res: res, key: key, query: query, path: path}, true
}

// routeForKind finds the route whose bookkeeping this kind names.
func (e *Engine) routeForKind(kind string) *Route {
	for _, rt := range e.spec.Routes {
		if routeKind(rt) == kind {
			return rt
		}
	}
	return nil
}

// fillPath substitutes a key back into the route's path pattern.
func (rt *Route) fillPath(key map[string]string) (string, bool) {
	segs := strings.Split(strings.Trim(rt.Path, "/"), "/")
	for i, s := range segs {
		if len(s) < 3 || s[0] != '{' || s[len(s)-1] != '}' {
			continue
		}
		param := s[1 : len(s)-1]
		v, ok := key[rt.column(param)]
		if !ok || v == "" {
			return "", false
		}
		segs[i] = url.PathEscape(v)
	}
	return "/" + strings.Join(segs, "/"), true
}

// parseKeyString reverses keyString.
//
// The are each other's inverse and must stay so: a sweep that decoded a key
// differently from the way a read encoded it would refresh a row nobody reads
// and leave the they do read stale. The declared components come in
// declaration order, then the sorted name=value extras.
func parseKeyString(res *Resource, s string) (map[string]string, bool) {
	parts := strings.Split(s, "/")
	if len(parts) < len(res.Keys) {
		return nil, false
	}
	key := make(map[string]string, len(parts))
	for i, k := range res.Keys {
		v, err := url.PathUnescape(parts[i])
		if err != nil {
			return nil, false
		}
		key[k.Name] = v
	}
	for _, extra := range parts[len(res.Keys):] {
		name, value, ok := strings.Cut(extra, "=")
		if !ok {
			return nil, false
		}
		n, err := url.PathUnescape(name)
		if err != nil {
			return nil, false
		}
		v, err := url.PathUnescape(value)
		if err != nil {
			return nil, false
		}
		key[n] = v
	}
	return key, true
}
