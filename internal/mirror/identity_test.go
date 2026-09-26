package mirror

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identityUpstream is a fake upstream for the identity tests. GET
// /app accepts one JWT.
type identityUpstream struct {
	appCalls    atomic.Int32
	secretCalls atomic.Int32
	leaked      atomic.Bool
	appStatus   atomic.Int32
}

func (u *identityUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Mirror-Identity") != "" {
		u.leaked.Store(true)
	}
	switch r.URL.Path {
	case "/app":
		u.appCalls.Add(1)
		if s := u.appStatus.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		if r.Header.Get("Authorization") != "Bearer good-jwt" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"id":42,"slug":"my-app"}`))
	case "/secrets/1":
		u.secretCalls.Add(1)
		if r.Header.Get("Authorization") != "token t1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"id":"1","title":"classified"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func withIdentity(s *Spec) {
	s.Upstream.Forward = append(s.Upstream.Forward, "Authorization")
	s.Resources = append(s.Resources, &Resource{
		Name:   "secret",
		Store:  StoreColumns,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "id", From: "id"}},
		Fields: []Field{{Name: "title", Type: FieldText, From: "title"}},
		Reveal: &Reveal{
			Probe:    &Probe{Method: http.MethodGet, Path: "/secrets/{id}"},
			GrantTTL: time.Hour,
			DenyTTL:  time.Minute,
		},
	})
	s.Routes = append(s.Routes, &Route{Method: "GET", Path: "/secrets/{id}", Resource: "secret"})
	s.Identity = &Identity{Header: "X-Mirror-Identity", Path: "/app", Scheme: "Bearer", ID: "id", Name: "slug", Prefix: "app"}
}

func getSecret(e *Engine, token, jwt string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/secrets/1", nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	if jwt != "" {
		req.Header.Set("X-Mirror-Identity", jwt)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestIdentity_GrantSurvivesTokenRotation(t *testing.T) {
	up := &identityUpstream{}
	e, _ := newTestEngine(t, up, withIdentity)

	first := getSecret(e, "token t1", "good-jwt")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Contains(t, first.Body.String(), "classified")

	// The rotated token alone cannot read this.
	secretsBefore := up.secretCalls.Load()
	rotated := getSecret(e, "token t2", "good-jwt")
	assert.Equal(t, http.StatusOK, rotated.Code, "a grant earned as app:42 must survive a new installation token")
	assert.Equal(t, secretsBefore, up.secretCalls.Load(), "the grant answers, so neither a probe nor a fetch goes out")

	// Without the assertion, t2 is its own principal and has no grant.
	bare := getSecret(e, "token t2", "")
	assert.Equal(t, http.StatusNotFound, bare.Code, "the fingerprint principal of t2 must prove access itself")

	assert.False(t, up.leaked.Load(), "the identity header must never reach the upstream")
}

func TestIdentity_VerdictIsCached(t *testing.T) {
	up := &identityUpstream{}
	e, _ := newTestEngine(t, up, withIdentity)

	getSecret(e, "token t1", "good-jwt")
	getSecret(e, "token t1", "good-jwt")
	assert.EqualValues(t, 1, up.appCalls.Load(), "a verified assertion is reused, not re-verified per request")
}

func TestIdentity_ForgedAssertionIsRefused(t *testing.T) {
	up := &identityUpstream{}
	e, _ := newTestEngine(t, up, withIdentity)

	rec := getSecret(e, "token t1", "forged-jwt")
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a refused assertion must not fall back to the token fingerprint")
	assert.Zero(t, up.secretCalls.Load(), "nothing is revealed or fetched for a caller whose identity failed")

	getSecret(e, "token t1", "forged-jwt")
	assert.EqualValues(t, 2, up.appCalls.Load(), "a refusal is not cached, so a fixed assertion is not locked out")
}

func TestIdentity_TransientFailureIsNotCached(t *testing.T) {
	up := &identityUpstream{}
	e, _ := newTestEngine(t, up, withIdentity)

	up.appStatus.Store(http.StatusServiceUnavailable)
	rec := getSecret(e, "token t1", "good-jwt")
	assert.Equal(t, http.StatusBadGateway, rec.Code, "an outage is not a refusal of the assertion")

	up.appStatus.Store(0)
	rec = getSecret(e, "token t1", "good-jwt")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A caller with no credential must never get the mirror's own.
func TestTokenlessCallerDoesNotRideTheBackgroundCredential(t *testing.T) {
	up := &identityUpstream{}
	e, _ := newTestEngine(t, up, withIdentity, func(s *Spec) {
		s.Upstream.Headers = append(s.Upstream.Headers, Header{Name: "Authorization", Value: "token t1"})
	})

	rec := getSecret(e, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"the probe must go out with no credential, not with the static background one")
}

func TestWhoamiKeepsAUsersGrantsAcrossTokens(t *testing.T) {
	var whoCalls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			whoCalls.Add(1)
			switch r.Header.Get("Authorization") {
			case "token t1", "token t2":
				w.Write([]byte(`{"id":7}`))
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		case "/secrets/1":
			if r.Header.Get("Authorization") != "token t1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(`{"id":"1","title":"classified"}`))
		}
	}), withIdentity, func(s *Spec) {
		s.Whoami = &Whoami{Path: "/user", ID: "id", Prefix: "user"}
	})

	require.Equal(t, http.StatusOK, getSecret(e, "token t1", "").Code)
	assert.Equal(t, http.StatusOK, getSecret(e, "token t2", "").Code,
		"t2 belongs to the same user, whose grant t1 earned")
	assert.Equal(t, http.StatusNotFound, getSecret(e, "token other", "").Code,
		"a credential the upstream will not name keeps its own fingerprint and proves nothing")

	before := whoCalls.Load()
	getSecret(e, "token t2", "")
	assert.Equal(t, before, whoCalls.Load(), "the owner of a credential is cached")
}

func TestValidate_Identity(t *testing.T) {
	base := func() *Spec {
		s := testSpec("https://api.example.com")
		withIdentity(s)
		return s
	}
	require.NoError(t, base().validate())

	s := base()
	s.Upstream.Forward = append(s.Upstream.Forward, "x-mirror-identity")
	err := s.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leave for the upstream")

	s = base()
	s.Identity.Prefix = "app:x"
	require.Error(t, s.validate())

	s = base()
	s.Identity.ID = ""
	require.Error(t, s.validate())

	s = base()
	require.NoError(t, s.validate())
	assert.Equal(t, http.MethodGet, s.Identity.Method)
	assert.Equal(t, "Authorization", s.Identity.Send)
}
