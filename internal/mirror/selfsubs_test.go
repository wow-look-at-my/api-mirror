package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func asCaller(t *testing.T, e *Engine, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	e.ServeHTTP(rec, r)
	return rec
}

func TestSelfService_ACallerManagesOnlyTheirOwnSubscriptions(t *testing.T) {
	e := notifyEngine(t)
	const path = "/_mirror/subs"

	created := asCaller(t, e, http.MethodPost, path, "token alice", `{"url":"https://alice.example/hook"}`)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var sub subscriptionCreated
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &sub))
	assert.NotEmpty(t, sub.Secret)
	assert.Equal(t, "token:"+fingerprint("token alice"), sub.Principal)

	var own []Subscription
	require.NoError(t, json.Unmarshal(asCaller(t, e, http.MethodGet, path, "token alice", "").Body.Bytes(), &own))
	assert.Len(t, own, 1)
	var theirs []Subscription
	require.NoError(t, json.Unmarshal(asCaller(t, e, http.MethodGet, path, "token bob", "").Body.Bytes(), &theirs))
	assert.Empty(t, theirs, "a caller lists their own subscriptions only")

	assert.Equal(t, http.StatusNotFound, asCaller(t, e, http.MethodDelete, path+"/"+sub.ID, "token bob", "").Code,
		"another caller's id reads as absent")
	assert.Equal(t, http.StatusNoContent, asCaller(t, e, http.MethodPatch, path+"/"+sub.ID, "token alice", `{"active":true}`).Code)
	assert.Equal(t, http.StatusNoContent, asCaller(t, e, http.MethodDelete, path+"/"+sub.ID, "token alice", "").Code)

	assert.Equal(t, http.StatusUnauthorized, asCaller(t, e, http.MethodGet, path, "", "").Code,
		"a subscription belongs to a caller, and the dashboard token is not one")
}

func TestGate_APrivateDeliveryReachesOnlyAProvenPrincipal(t *testing.T) {
	f := newFixture(t, unreachable(t))
	ctx := context.Background()
	f.putRow(t, "wow", "secret", "private")
	key := repoKey("wow", "secret")
	require.NoError(t, f.store.RecordGrant(ctx, Grant{
		Principal: "user:1", Resource: proofScope, Key: "/repos/wow/secret",
		Source: grantSourceProbe, ExpiresAt: time.Now().Add(time.Hour),
	}))

	assert.True(t, f.rv.Visible(ctx, "user:1", f.res, key))
	assert.False(t, f.rv.Visible(ctx, "user:2", f.res, key), "no proof is no notification, and the gate never probes")
	assert.False(t, f.rv.Visible(ctx, "", f.res, key))

	f.putRow(t, "wow", "open", "public")
	assert.True(t, f.rv.Visible(ctx, "user:2", f.res, repoKey("wow", "open")))
	assert.Zero(t, f.calls.Load())
}

func TestProof_APublicRepositoryAnswersTheProbeOfEverythingInIt(t *testing.T) {
	spec := revealSpec("http://upstream.invalid")
	spec.Resources = append(spec.Resources, &Resource{
		Name: "pull", Store: StoreDocument, TTL: time.Hour,
		Keys: []Key{{Name: "owner"}, {Name: "repo"}, {Name: "number"}},
		Reveal: &Reveal{
			Probe:    &Probe{Method: http.MethodGet, Path: "/repos/{owner}/{repo}"},
			GrantTTL: time.Hour, DenyTTL: time.Minute,
		},
	})
	spec.Routes = []*Route{{Method: http.MethodGet, Path: "/repos/{owner}/{repo}", Resource: "repo"}}
	store := openStore(t, dbPath(t), spec)
	up, err := NewUpstreamer(spec, map[string]any{}, nil)
	require.NoError(t, err)
	rv := NewRevealer(store, up, map[string]any{})
	repo, _ := store.Resource("repo")
	pull, _ := store.Resource("pull")
	ctx := context.Background()
	require.NoError(t, store.Put(ctx, repo, Row{"owner": "wow", "repo": "open", "visibility": "public"}, time.Now()))
	require.NoError(t, store.Put(ctx, repo, Row{"owner": "wow", "repo": "shut", "visibility": "private"}, time.Now()))

	assert.True(t, rv.Visible(ctx, "user:2", pull, map[string]string{"owner": "wow", "repo": "open", "number": "1"}))
	assert.False(t, rv.Visible(ctx, "user:2", pull, map[string]string{"owner": "wow", "repo": "shut", "number": "1"}))
}

func TestProof_OneProbeCoversEveryResourceThatAsksTheSameQuestion(t *testing.T) {
	f := newFixture(t, respondWith(http.StatusOK))
	f.putRow(t, "wow", "secret", "private")
	_, err := f.allow(t, "user:1", repoKey("wow", "secret"))
	require.NoError(t, err)

	rule := *f.res.Reveal
	rule.Public = ""
	sibling := &Resource{Name: "pull", Keys: []Key{{Name: "owner"}, {Name: "repo"}, {Name: "number"}}, Reveal: &rule}
	v, err := f.rv.Allow(context.Background(), "user:1", sibling,
		map[string]string{"owner": "wow", "repo": "secret", "number": "5"}, http.Header{})
	require.NoError(t, err)
	assert.True(t, v.Allowed)
	assert.EqualValues(t, 1, f.calls.Load(), "the repository was already proven")
}
