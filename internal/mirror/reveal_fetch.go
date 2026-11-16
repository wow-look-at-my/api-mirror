// reveal_fetch.go holds the denial a refused fetch earns.

package mirror

import (
	"context"
	"net/http"
)

// fetchScope prefixes the scope of a denial one key's fetch earned.
const fetchScope = "fetch:"

// FetchRefused remembers a refusal the upstream gave this caller's own fetch.
//
// A forbidden answer does not say whether the caller lost the whole proof or
// lacks the permission of one endpoint. The probe says which, so it is asked
// again. A refused probe denies the proof, and every resource behind it. A
// passing probe keeps the proof, and the refusal belongs to this credential
// and this key alone.
func (rv *Revealer) FetchRefused(ctx context.Context, principal string, res *Resource, key map[string]string, status int, forward http.Header) {
	expires := rv.now().Add(res.Reveal.DenyTTL)
	scope, k, err := rv.proofOf(res, key)
	if err != nil {
		logf("reveal: fetch refused %s: %v", res.Name, err)
		return
	}
	if res.Reveal.Probe == nil {
		// Without a probe the proof is this key's own, so the refusal is its denial.
		rv.remember(ctx, principal, scope, k, status, expires)
		return
	}
	if status != http.StatusForbidden {
		// A missing thing behind a probe is not a refusal. The route chose not to store it, so the next request asks again.
		return
	}
	if v, err := rv.probe(ctx, principal, res, key, scope, k, forward); err == nil && !v.Allowed {
		// The probe remembered its own refusal.
		return
	}
	who := fetchCaller(principal, forward)
	if who == "" {
		return
	}
	if err := rv.store.RecordDenial(ctx, who, fetchScope+res.Name, keyString(res, key), status, expires); err != nil {
		logf("reveal: record fetch denial %s %s: %v", who, res.Name, err)
	}
}

// fetchDenied replays a live refusal of this credential's fetch of this key.
func (rv *Revealer) fetchDenied(ctx context.Context, principal string, res *Resource, key map[string]string, forward http.Header) (int, bool, error) {
	who := fetchCaller(principal, forward)
	if who == "" {
		return 0, false, nil
	}
	return rv.store.Denial(ctx, who, fetchScope+res.Name, keyString(res, key), rv.now())
}

// fetchCaller names who a fetch refusal belongs to: the credential, because
// an endpoint's permission is the token's and not its owner's.
func fetchCaller(principal string, forward http.Header) string {
	if p := principalFor(forward.Get("Authorization")); p != "" {
		return p
	}
	return principal
}
