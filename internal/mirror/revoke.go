package mirror

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// revokeRefused drops every stored answer that handed out the credential the
// upstream just refused.
//
// A rate-limit refusal says nothing about the credential, so it revokes
// nothing.
func (e *Engine) revokeRefused(ctx context.Context, auth string, status int, header http.Header) {
	if auth == "" || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return
	}
	if e.up.RateLimited(&Answer{Status: status, Header: header}) {
		return
	}
	token := bearerOf(auth)
	for _, res := range e.spec.Resources {
		if res.RevokeOn == "" {
			continue
		}
		keys, err := e.store.keysWhere(ctx, res, "json_extract(document, ?) = ?", "$."+res.RevokeOn, token)
		if err != nil {
			logf("revoke %s: %v", res.Name, err)
			continue
		}
		for _, k := range keys {
			if _, err := e.store.Delete(ctx, res, k); err != nil {
				logf("revoke %s: %v", res.Name, err)
			}
			if err := e.store.Forget(ctx, res, k); err != nil {
				logf("revoke %s: %v", res.Name, err)
			}
		}
	}
}

// revokeOnResponse is the passthrough's hook into revokeRefused.
func (e *Engine) revokeOnResponse(resp *http.Response) error {
	if resp.Request != nil {
		e.revokeRefused(resp.Request.Context(), resp.Request.Header.Get("Authorization"), resp.StatusCode, resp.Header)
	}
	return stripUpstreamCORS(resp)
}

// keysWhere reads the full keys of the rows a predicate selects.
func (s *Store) keysWhere(ctx context.Context, r *Resource, where string, args ...any) ([]map[string]string, error) {
	names := keyNames(r)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s`,
		strings.Join(names, ", "), resourceTable(r.Name), where), args...)
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", r.Name, err)
	}
	defer rows.Close()
	var out []map[string]string
	vals := make([]string, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("select %s: %w", r.Name, err)
		}
		key := make(map[string]string, len(names))
		for i, name := range names {
			key[name] = vals[i]
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
