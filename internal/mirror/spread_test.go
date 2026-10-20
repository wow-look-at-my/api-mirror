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

// withIdentity declares the private "secret" resource a listing spreads into,
// and leaves the caller named by its credential. No <identity> rule resolves
// it any other way. A grant is keyed by the fingerprint.
func withIdentity(s *Spec) {
	s.Identity = nil
	s.Resources = append(s.Resources, &Resource{
		Name:   "secret",
		Store:  StoreColumns,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "id", From: "id"}},
		Fields: []Field{{Name: "title", Type: FieldText, From: "title"}},
		Reveal: &Reveal{Public: "false", GrantTTL: time.Hour},
	})
}

// withListing adds a private "secret" resource and a caller's listing of
// secrets that spreads into it.
func withListing(s *Spec) {
	withIdentity(s)
	s.Resources = append(s.Resources, &Resource{
		Name:   "listing",
		Store:  StoreDocument,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "token_fp", Credential: true}},
		Reveal: &Reveal{Credential: true},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "GET", Path: "/mine", Resource: "listing",
		Spreads: []*Spread{{Resource: "secret", Items: "items", Absorb: true, Grant: true}},
	})
}

func TestSpreadGrantsTheCallerEveryListedItem(t *testing.T) {
	var probes atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mine":
			w.Write([]byte(`{"items":[{"id":"1","title":"classified"},{"title":"no key"}]}`))
		case "/secrets/1":
			probes.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}), withListing)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "token t9")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusOK, get("/mine").Code)

	secret := get("/secrets/1")
	assert.Equal(t, http.StatusOK, secret.Code, "the listing proved access, so no probe is needed")
	assert.Contains(t, secret.Body.String(), "classified", "the listed item was stored as truth")
	assert.Zero(t, probes.Load())

	res, ok := e.store.Resource("secret")
	require.True(t, ok)
	has, err := e.store.HasGrant(context.Background(), tokenPrincipal(&http.Request{Header: http.Header{"Authorization": {"token t9"}}}), "secret",
		keyString(res, map[string]string{"id": "1"}), time.Now())
	require.NoError(t, err)
	assert.True(t, has)
}

func TestValidateSpread(t *testing.T) {
	s := testSpec("https://api.example.com")
	withListing(s)
	require.NoError(t, s.validate())

	s = testSpec("https://api.example.com")
	withListing(s)
	s.Routes[len(s.Routes)-1].Spreads[0].Resource = "nope"
	require.Error(t, s.validate())

	s = testSpec("https://api.example.com")
	withListing(s)
	s.Routes[len(s.Routes)-1].Spreads[0].Resource = "widget"
	err := s.validate()
	require.Error(t, err, "widget is public with no grant ttl, so a grant on it would never be read")
	assert.Contains(t, err.Error(), "no <grant ttl>")
}
