package mirror

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// The reveal layer: who may be TOLD a stored fact.

// grantSourceProbe labels a grant a probe earned. The source is what keeps a
const grantSourceProbe = "probe"

// DenyReason says which rung of the ladder refused. The vocabulary is closed so
// the dashboard can tally refusals: "denied" with no reason is a number an
// operator cannot act on.
type DenyReason string

const (
	DenyProbe    DenyReason = "probe-refused" // the upstream refused this caller
	DenyCached   DenyReason = "cached-denial" // a refusal replayed from the deny cache
	DenyNoProof  DenyReason = "no-proof"      // not public and nothing left to ask
	DenyUpstream DenyReason = "probe-inconclusive"
)

// Verdict is the reveal decision for one read.
type Verdict struct {
	Allowed bool
	// Status is the answer when the read is refused. It is the upstream's own
	Status int
	// Cached reports a refusal replayed from the deny cache rather than one a
	Cached bool
	// Reason names the rung that refused, for the dashboard's refusal tally.
	Reason DenyReason
}

// Revealer decides what a principal may be shown.
type Revealer struct {
	store *Store
	up    *Upstreamer
	vars  map[string]any
	now   func() time.Time
}

// NewRevealer builds the gate every cached read passes through.
func NewRevealer(store *Store, up *Upstreamer, vars map[string]any) *Revealer {
	return &Revealer{store: store, up: up, vars: vars, now: time.Now}
}

// Allow decides whether this principal may be shown this resource key.
//
// The ladder is public, then grant, then cached denial, then probe, and each
// rung is a different mechanism rather than a shortcut for the one below it. A
// failure anywhere refuses: a store error, a template that cannot render and a
// row that is not there all mean the engine has no proof, and no proof is a
// refusal.
func (rv *Revealer) Allow(ctx context.Context, principal string, res *Resource, key map[string]string, forward http.Header) (Verdict, error) {
	if res.Reveal == nil {
		// Load-time validation rejects this. The runtime still refuses rather
		return refuse(http.StatusNotFound), fmt.Errorf("resource %q has no reveal rule", res.Name)
	}
	if res.Reveal.Credential {
		// A row keyed by the caller's own credential proves itself.
		return Verdict{Allowed: true}, nil
	}

	public, err := rv.public(ctx, res, key)
	if err != nil {
		return refuse(http.StatusBadGateway), err
	}
	if public {
		return Verdict{Allowed: true}, nil
	}

	k := keyString(res, key)
	now := rv.now()

	// A caller the engine could not name holds no proof and can be given none.
	// Their probe still runs -- their own credential answers it -- but nothing
	// remembers the answer, so every request pays for one.
	if principal != "" {
		ok, err := rv.store.HasGrant(ctx, principal, res.Name, k, now)
		if err != nil {
			return refuse(http.StatusBadGateway), err
		}
		if ok {
			return Verdict{Allowed: true}, nil
		}

		status, found, err := rv.store.Denial(ctx, principal, res.Name, k, now)
		if err != nil {
			return refuse(http.StatusBadGateway), err
		}
		if found {
			return Verdict{Status: status, Cached: true, Reason: DenyCached}, nil
		}
	}

	return rv.probe(ctx, principal, res, key, k, forward)
}

// public evaluates the resource's public predicate against the stored row.
//
// Every way of not knowing reads as private. A row that is absent proves
// nothing, an unset column renders empty and an empty string is not truthy, so
// the fast path opens only on a predicate that positively said yes.
func (rv *Revealer) public(ctx context.Context, res *Resource, key map[string]string) (bool, error) {
	if res.Reveal.Public == "" {
		return false, nil
	}
	var row Row
	if completeKey(res, key) {
		// A partial key names many rows, and one row's visibility is not the
		var err error
		if row, err = rv.store.Get(ctx, res, key); err != nil {
			return false, fmt.Errorf("reveal: read %s: %w", res.Name, err)
		}
	}
	// A missing row is not a refusal here, it is an empty row. The distinction
	out, err := renderString(res.Reveal.Public, rv.context(key, row))
	if err != nil {
		// A predicate that cannot render decides nothing. It is a spec bug, so
		logf("reveal: %s <public> did not render: %v", res.Name, err)
		return false, nil
	}
	return isTruthy(out), nil
}

// probe asks the upstream, with the CALLER's own forwarded credential, whether
// this caller may read this key.
//
// The answer is sorted into three kinds, and the kind decides what is
// remembered. A 2xx earns a grant. An authoritative refusal is cached for the
// deny window. Everything else -- a server failure, a rate-limit refusal, a
// transport error, a status that says nothing about access -- is remembered
// nowhere, because a bad minute upstream must not lock a caller out of their
// own data for the whole deny window.
func (rv *Revealer) probe(ctx context.Context, principal string, res *Resource, key map[string]string, k string, forward http.Header) (Verdict, error) {
	rule := res.Reveal
	if rule.Probe == nil {
		// The predicate said no and there is nothing left to ask. The refusal
		return Verdict{Status: http.StatusNotFound, Reason: DenyNoProof}, nil
	}
	path, err := rv.probePath(res, key)
	if err != nil {
		return refuse(http.StatusBadGateway), err
	}

	// A probe is its own lane on the chart. It is upstream traffic the consumer
	// never asked for, so folding it into the fetch lane hides what proving
	// access actually costs.
	ans, err := rv.up.Call(withLane(ctx, LaneProbe, principal, res.Name), rule.Probe.Method, path, rv.context(key, nil), forward)
	if err != nil {
		return refuse(http.StatusBadGateway), fmt.Errorf("reveal probe %s: %w", res.Name, err)
	}
	now := rv.now()

	switch {
	case ans.Status >= 200 && ans.Status < 300:
		if principal != "" {
			g := Grant{
				Principal: principal,
				Resource:  res.Name,
				Key:       k,
				Source:    grantSourceProbe,
				ExpiresAt: now.Add(rule.GrantTTL),
			}
			if err := rv.store.RecordGrant(ctx, g); err != nil {
				return refuse(http.StatusBadGateway), err
			}
		}
		return Verdict{Allowed: true}, nil

	case rv.up.Transient(ans):
		// Checked before the authoritative case on purpose: a rate-limited 403
		// wears the same status as a real refusal and means the opposite.
		return refuse(http.StatusBadGateway),
			fmt.Errorf("reveal probe %s: upstream answered %d, which states nothing about access", res.Name, ans.Status)

	case ans.Status == http.StatusNotFound, ans.Status == http.StatusForbidden:
		rv.remember(ctx, principal, res, k, ans.Status, now.Add(rule.DenyTTL))
		return Verdict{Status: ans.Status, Reason: DenyProbe}, nil

	default:
		return refuse(http.StatusBadGateway),
			fmt.Errorf("reveal probe %s: upstream answered %d, which states nothing about access", res.Name, ans.Status)
	}
}

// remember caches one authoritative refusal, and on a 403 drops the proof it
// contradicts.
//
// A 403 is the upstream stating that this caller may not read this. A 404 is
// not: it cannot be told apart from a missing thing inside something the caller
// CAN see, so it never revokes.
//
// A bookkeeping failure here does not change the verdict. The upstream proved
// the refusal; failing to write it down costs one extra probe next time, and
// the log says so.
func (rv *Revealer) remember(ctx context.Context, principal string, res *Resource, k string, status int, expires time.Time) {
	if principal == "" {
		return
	}
	if err := rv.store.RecordDenial(ctx, principal, res.Name, k, status, expires); err != nil {
		logf("reveal: record denial %s %s/%s: %v", principal, res.Name, k, err)
	}
	if status != http.StatusForbidden {
		return
	}
	if _, err := rv.store.RevokeGrant(ctx, principal, res.Name, k); err != nil {
		logf("reveal: revoke grant %s %s/%s: %v", principal, res.Name, k, err)
	}
}

// RenewOn2xx renews a caller's proof after the engine fetched this key with
// that caller's own credential.
//
// The upstream just answered them yes, which is the same proof a probe buys. A
// steady consumer therefore never ages out, and never pays for a probe that
// re-asks a question the fetch already answered.
func (rv *Revealer) RenewOn2xx(ctx context.Context, principal string, res *Resource, key map[string]string, status int) {
	if principal == "" || status < 200 || status >= 300 {
		return
	}
	if res.Reveal == nil || res.Reveal.GrantTTL <= 0 {
		return
	}
	k := keyString(res, key)
	g := Grant{
		Principal: principal,
		Resource:  res.Name,
		Key:       k,
		Source:    grantSourceProbe,
		ExpiresAt: rv.now().Add(res.Reveal.GrantTTL),
	}
	if err := rv.store.RecordGrant(ctx, g); err != nil {
		logf("reveal: renew grant %s %s/%s: %v", principal, res.Name, k, err)
	}
}

// probePath renders the probe's path for one key.
//
// Both spellings a spec plausibly uses work: the {name} placeholders route
// paths already use, and a template over .key.<name> and .var.<name>. A
// placeholder left over after both means a key nothing supplied, which is an
// error rather than a request to a path with a brace in it.
func (rv *Revealer) probePath(res *Resource, key map[string]string) (string, error) {
	path := res.Reveal.Probe.Path
	for _, k := range res.Keys {
		v, ok := key[k.Name]
		if !ok {
			continue
		}
		path = strings.ReplaceAll(path, "{"+k.Name+"}", v)
	}
	out, err := renderString(path, rv.context(key, nil))
	if err != nil {
		return "", fmt.Errorf("reveal: %s probe path: %w", res.Name, err)
	}
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("reveal: %s probe path %q still names a key nothing supplied", res.Name, out)
	}
	if !strings.HasPrefix(out, "/") {
		return "", fmt.Errorf("reveal: %s probe path %q is not absolute", res.Name, out)
	}
	return out, nil
}

// context is what a reveal template sees: the spec's own vars, plus the key
// this read addresses and the row it found.
func (rv *Revealer) context(key map[string]string, row Row) map[string]any {
	if row == nil {
		row = Row{}
	}
	out := make(map[string]any, len(rv.vars)+2)
	for k, v := range rv.vars {
		out[k] = v
	}
	out["key"] = key
	out["row"] = row
	return out
}

// refuse builds a not-allowed verdict. Every failure path goes through it, so
func refuse(status int) Verdict {
	return Verdict{Status: status, Reason: DenyUpstream}
}

// keyString renders a resource key as the one string every path names it by:
// the grant, the denial and the freshness marker.
//
// A key becomes text here and nowhere else, so a grant recorded by a probe and
// a grant looked up by a read agree. Components hold their DECLARED position
// and are escaped, which keeps an absent component and a component containing
// the separator distinguishable: two different keys cannot render one string.
func keyString(res *Resource, key map[string]string) string {
	parts := make([]string, 0, len(key)+len(res.Keys))
	named := make([]string, 0, len(res.Keys))
	for _, k := range res.Keys {
		parts = append(parts, url.PathEscape(key[k.Name]))
		named = append(named, k.Name)
	}
	// A list route may also select by a stored FIELD -- every post by an
	extra := make([]string, 0, len(key))
	for name := range key {
		if !slices.Contains(named, name) {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		parts = append(parts, url.PathEscape(name)+"="+url.PathEscape(key[name]))
	}
	return strings.Join(parts, "/")
}

// completeKey reports whether every declared key component is supplied.
func completeKey(res *Resource, key map[string]string) bool {
	for _, k := range res.Keys {
		if _, ok := key[k.Name]; !ok {
			return false
		}
	}
	return true
}
