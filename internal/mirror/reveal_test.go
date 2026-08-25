package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revealSpec is the spec every reveal test opens against: a public predicate
// over one stored column, and a probe for everything the predicate refuses.
func revealSpec(base string) *Spec {
	return &Spec{
		Name:     "reveal",
		Upstream: Upstream{Base: base, Forward: []string{"Authorization"}},
		Resources: []*Resource{{
			Name:   "repo",
			Store:  StoreColumns,
			TTL:    time.Hour,
			Keys:   []Key{{Name: "owner"}, {Name: "repo"}},
			Fields: []Field{{Name: "visibility", Type: FieldText, From: "visibility"}},
			Reveal: &Reveal{
				Public:   `{{ eq .row.visibility "public" }}`,
				Probe:    &Probe{Method: http.MethodGet, Path: "/repos/{owner}/{repo}"},
				GrantTTL: time.Hour,
				DenyTTL:  5 * time.Minute,
			},
		}},
	}
}

// fixture is one reveal layer wired to a fake upstream that counts what it is
type fixture struct {
	rv    *Revealer
	store *Store
	res   *Resource
	calls atomic.Int64
	auth  atomic.Value
}

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.auth.Store(r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	spec := revealSpec(srv.URL)
	f.store = openStore(t, dbPath(t), spec)
	up, err := NewUpstreamer(spec, map[string]any{}, nil)
	require.NoError(t, err)
	res, ok := f.store.Resource("repo")
	require.True(t, ok)
	f.res = res
	f.rv = NewRevealer(f.store, up, map[string]any{})
	return f
}

// respondWith answers every probe with one status and no body.
func respondWith(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// unreachable fails the test if the upstream is asked at all.
func unreachable(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be asked: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *fixture) putRow(t *testing.T, owner, repo, visibility string) {
	t.Helper()
	row := Row{"owner": owner, "repo": repo, "visibility": visibility}
	require.NoError(t, f.store.Put(context.Background(), f.res, row, time.Now()))
}

func (f *fixture) allow(t *testing.T, principal string, key map[string]string) (Verdict, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer caller-token")
	return f.rv.Allow(context.Background(), principal, f.res, key, h)
}

// grantRows counts the stored grant rows for a key, expired ones included.
// HasGrant filters on expiry, so it cannot tell a revoked grant from a lapsed
// one, and the revoke rules are exactly about that difference.
func (f *fixture) grantRows(t *testing.T, principal, key string) int {
	t.Helper()
	var n int
	err := f.store.db.QueryRow(
		`SELECT COUNT(*) FROM mirror_grant WHERE principal = ? AND resource = ? AND key = ?`,
		principal, "repo", key).Scan(&n)
	require.NoError(t, err)
	return n
}

func repoKey(owner, repo string) map[string]string {
	return map[string]string{"owner": owner, "repo": repo}
}

func TestPublicRowNeedsNoPrincipalAndNoCall(t *testing.T) {
	f := newFixture(t, unreachable(t))
	f.putRow(t, "wow", "api-mirror", "public")

	v, err := f.allow(t, "", repoKey("wow", "api-mirror"))
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.Zero(t, f.calls.Load(), "the public rung must not reach the upstream")
}

// Nothing stored proves nothing. The read must fall through rather than read as
// public because the predicate found no row to disagree with.
func TestMissingRowIsNotPublic(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusNotFound))

	v, err := f.allow(t, "user:1", repoKey("wow", "never-seen"))
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, http.StatusNotFound, v.Status)
	assert.Equal(t, int64(1), f.calls.Load(), "an absent row must fall to the probe")
}

// An unset column renders empty, and empty is not truthy.
func TestUnsetVisibilityIsNotPublic(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusNotFound))
	require.NoError(t, f.store.Put(context.Background(), f.res,
		Row{"owner": "wow", "repo": "unknown", "visibility": nil}, time.Now()))

	v, err := f.allow(t, "user:1", repoKey("wow", "unknown"))
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, int64(1), f.calls.Load())
}

func TestLiveGrantAllowsWithoutCall(t *testing.T) {
	f := newFixture(t, unreachable(t))
	f.putRow(t, "wow", "secret", "private")
	require.NoError(t, f.store.RecordGrant(context.Background(), Grant{
		Principal: "user:1", Resource: "repo", Key: keyString(f.res, repoKey("wow", "secret")),
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(time.Hour),
	}))

	v, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.Zero(t, f.calls.Load())
}

func TestExpiredGrantDoesNotAllow(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusForbidden))
	f.putRow(t, "wow", "secret", "private")
	require.NoError(t, f.store.RecordGrant(context.Background(), Grant{
		Principal: "user:1", Resource: "repo", Key: keyString(f.res, repoKey("wow", "secret")),
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(-time.Hour),
	}))

	v, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.False(t, v.Allowed, "a lapsed proof is no proof")
	assert.Equal(t, http.StatusForbidden, v.Status)
	assert.Equal(t, int64(1), f.calls.Load(), "an expired grant must be re-proven")
}

// A grant proves one principal's access, never everyone's.
func TestGrantIsPerPrincipal(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusNotFound))
	f.putRow(t, "wow", "secret", "private")
	require.NoError(t, f.store.RecordGrant(context.Background(), Grant{
		Principal: "user:1", Resource: "repo", Key: keyString(f.res, repoKey("wow", "secret")),
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(time.Hour),
	}))

	v, err := f.allow(t, "user:2", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, int64(1), f.calls.Load())
}

func TestCachedDenialReplaysWithoutCall(t *testing.T) {
	f := newFixture(t, unreachable(t))
	f.putRow(t, "wow", "secret", "private")
	require.NoError(t, f.store.RecordDenial(context.Background(), "user:1", "repo",
		keyString(f.res, repoKey("wow", "secret")), http.StatusNotFound, time.Now().Add(time.Minute)))

	v, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, http.StatusNotFound, v.Status)
	assert.True(t, v.Cached, "a replayed refusal must say it was replayed")
	assert.Zero(t, f.calls.Load())
}

func TestProbe200RecordsGrantAndAllows(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusOK))
	f.putRow(t, "wow", "secret", "private")
	key := repoKey("wow", "secret")

	v, err := f.allow(t, "user:1", key)
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.Equal(t, int64(1), f.calls.Load())

	live, err := f.store.HasGrant(context.Background(), "user:1", "repo", keyString(f.res, key), time.Now())
	require.NoError(t, err)
	assert.True(t, live, "a proven probe must be remembered")

	// The proof is what stops the next read paying for the same question.
	v, err = f.allow(t, "user:1", key)
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.Equal(t, int64(1), f.calls.Load())
}

// A probe carries the CALLER's credential, which is the whole reason its answer
// proves anything about that caller.
func TestProbeForwardsCallerCredential(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusOK))
	f.putRow(t, "wow", "secret", "private")

	_, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.Equal(t, "Bearer caller-token", f.auth.Load())
}

// A 404 cannot be told apart from a missing thing inside something the caller
// CAN see, so it is remembered and it revokes nothing.
func TestProbe404DeniesWithoutRevoking(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusNotFound))
	key := repoKey("wow", "secret")
	k := keyString(f.res, key)
	require.NoError(t, f.store.RecordGrant(context.Background(), Grant{
		Principal: "user:1", Resource: "repo", Key: k,
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(-time.Hour),
	}))

	v, err := f.allow(t, "user:1", key)
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, http.StatusNotFound, v.Status)
	assert.False(t, v.Cached, "a fresh probe is not a replay")

	status, found, err := f.store.Denial(context.Background(), "user:1", "repo", k, time.Now())
	require.NoError(t, err)
	require.True(t, found, "an authoritative refusal must be remembered")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, 1, f.grantRows(t, "user:1", k), "a 404 must not revoke")
}

// A 403 is the upstream stating this caller may not read this, so the proof it
// contradicts has to go.
func TestProbe403DeniesAndRevokes(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusForbidden))
	key := repoKey("wow", "secret")
	k := keyString(f.res, key)
	require.NoError(t, f.store.RecordGrant(context.Background(), Grant{
		Principal: "user:1", Resource: "repo", Key: k,
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(-time.Hour),
	}))

	v, err := f.allow(t, "user:1", key)
	require.NoError(t, err)
	assert.False(t, v.Allowed)
	assert.Equal(t, http.StatusForbidden, v.Status)

	_, found, err := f.store.Denial(context.Background(), "user:1", "repo", k, time.Now())
	require.NoError(t, err)
	assert.True(t, found)
	assert.Zero(t, f.grantRows(t, "user:1", k), "a 403 must revoke the proof it contradicts")
}

// A rate-limited refusal wears a 403 and means the opposite of one. Caching it
// would lock a caller out of their own data for the whole deny window.
func TestRateLimited403CachesNothing(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})
	key := repoKey("wow", "secret")

	v, err := f.allow(t, "user:1", key)
	require.Error(t, err, "an undecidable probe must fail the request, not refuse it")
	assert.False(t, v.Allowed)

	_, found, err := f.store.Denial(context.Background(), "user:1", "repo", keyString(f.res, key), time.Now())
	require.NoError(t, err)
	assert.False(t, found, "a rate-limit refusal says nothing about access")
}

func TestProbe500CachesNothing(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusInternalServerError))
	key := repoKey("wow", "secret")

	v, err := f.allow(t, "user:1", key)
	require.Error(t, err)
	assert.False(t, v.Allowed)

	_, found, err := f.store.Denial(context.Background(), "user:1", "repo", keyString(f.res, key), time.Now())
	require.NoError(t, err)
	assert.False(t, found, "an outage must not be remembered as a refusal")
}

// A status that is neither proof nor refusal decides nothing either.
func TestProbe401CachesNothing(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusUnauthorized))
	key := repoKey("wow", "secret")

	v, err := f.allow(t, "user:1", key)
	require.Error(t, err)
	assert.False(t, v.Allowed)

	_, found, err := f.store.Denial(context.Background(), "user:1", "repo", keyString(f.res, key), time.Now())
	require.NoError(t, err)
	assert.False(t, found)
}

// With no probe declared, a predicate that says no is the end of the ladder.
func TestNoProbeRefuses(t *testing.T) {
	f := newFixture(t, unreachable(t))
	f.res.Reveal.Probe = nil
	f.putRow(t, "wow", "secret", "private")

	v, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)
	assert.False(t, v.Allowed, "no proof is a refusal, never an allow")
	assert.Equal(t, http.StatusNotFound, v.Status)
	assert.Zero(t, f.calls.Load())
}

// Load-time validation rejects this. The runtime must not assume it did.
func TestNilRevealIsAnError(t *testing.T) {
	f := newFixture(t, unreachable(t))
	f.res.Reveal = nil

	v, err := f.allow(t, "user:1", repoKey("wow", "api-mirror"))
	require.Error(t, err)
	assert.False(t, v.Allowed)
	assert.Zero(t, f.calls.Load())
}

func TestRenewOn2xxKeepsASteadyConsumerProven(t *testing.T) {
	f := newFixture(t, unreachable(t))
	ctx := context.Background()
	key := repoKey("wow", "secret")
	k := keyString(f.res, key)
	f.putRow(t, "wow", "secret", "private")

	f.rv.RenewOn2xx(ctx, "user:1", f.res, key, http.StatusOK)
	live, err := f.store.HasGrant(ctx, "user:1", "repo", k, time.Now())
	require.NoError(t, err)
	assert.True(t, live)

	v, err := f.allow(t, "user:1", key)
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.Zero(t, f.calls.Load())
}

// A non-2xx fetch proves nothing, and an unnamed caller has nothing to renew.
func TestRenewOn2xxIgnoresWhatProvesNothing(t *testing.T) {
	f := newFixture(t, unreachable(t))
	ctx := context.Background()
	key := repoKey("wow", "secret")
	k := keyString(f.res, key)

	f.rv.RenewOn2xx(ctx, "user:1", f.res, key, http.StatusNotFound)
	f.rv.RenewOn2xx(ctx, "", f.res, key, http.StatusOK)

	assert.Zero(t, f.grantRows(t, "user:1", k))
	assert.Zero(t, f.grantRows(t, "", k))
}

// A partial key names many rows, so one row's visibility cannot open the set.
func TestPartialKeyIsNotPublic(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusOK))
	// A list read is keyed by the parent, so its probe asks about the parent.
	f.res.Reveal.Probe.Path = "/orgs/{owner}"
	f.putRow(t, "wow", "api-mirror", "public")

	v, err := f.allow(t, "user:1", map[string]string{"owner": "wow"})
	require.NoError(t, err)
	assert.True(t, v.Allowed, "the probe proves what the predicate could not")
	assert.Equal(t, int64(1), f.calls.Load(), "a list read must not ride one row's visibility")
}
