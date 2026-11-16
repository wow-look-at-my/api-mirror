// invalidate.go holds forgetFreshness.

package mirror

import (
	"context"
	"net/url"
	"strings"
)

// forgetFreshness drops the bookkeeping of the answers an invalidation
// dropped. A stored refusal lives only there, so a delivery that says a
// refused thing now exists must clear it, or the refusal outlives the fact.
//
// A key that names a component after a gap is no text prefix. Those rows are
// gone all the same, and the serve path refetches each it finds fresh with no
// row.
func (i *Ingest) forgetFreshness(ctx context.Context, res *Resource, key map[string]string, every bool) {
	prefix, exact, ok := freshnessPrefix(res, key, every)
	if !ok {
		return
	}
	for _, kind := range []string{res.Name, res.Name + ":list"} {
		if exact != "" {
			if _, err := i.store.DeleteFreshness(ctx, kind, exact); err != nil {
				logf("invalidate %s: %v", kind, err)
			}
		}
		if _, err := i.store.DeleteFreshnessUnder(ctx, kind, prefix); err != nil {
			logf("invalidate %s: %v", kind, err)
		}
	}
}

// freshnessPrefix is the key text every answer under key starts with. A
// full key also names its own answer exactly. Every component ends at a
// separator, so "acme/tool" never matches "acme/tools".
func freshnessPrefix(res *Resource, key map[string]string, every bool) (prefix, exact string, ok bool) {
	if every {
		return "", "", true
	}
	var parts []string
	gap := false
	for _, k := range res.Keys {
		v, named := key[k.Name]
		if !named {
			gap = true
			continue
		}
		if gap {
			return "", "", false
		}
		parts = append(parts, url.PathEscape(v))
	}
	if len(parts) == 0 {
		return "", "", false
	}
	joined := strings.Join(parts, "/")
	if len(parts) == len(res.Keys) {
		exact = joined
	}
	return joined + "/", exact, true
}
