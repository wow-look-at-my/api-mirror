package mirror

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withAssertedMint declares an App-JWT mint keyed by the verified app.
func withAssertedMint(s *Spec) {
	s.Upstream.Forward = append(s.Upstream.Forward, "Authorization", "Content-Type")
	s.Identity = &Identity{
		TTL:       time.Hour,
		Assertion: &IdentityRule{Header: "X-Mirror-Identity", As: "Authorization", Scheme: "Bearer", Path: "/app", Principal: "app:{{ .doc.id }}"},
	}
	s.Resources = append(s.Resources, &Resource{
		Name: "install_token", Store: StoreDocument,
		Keys:   []Key{{Name: "app", Credential: true, ByPrincipal: true}, {Name: "id"}, {Name: "request"}},
		Reveal: &Reveal{Credential: true},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "POST", Path: "/app/installations/{id}/access_tokens", Resource: "install_token", BodyKey: "request", Assert: true,
	})
}

func TestAssert_ARotatedJWTOfTheSameAppFindsTheSameMint(t *testing.T) {
	var mints atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if r.URL.Path == "/app" {
			if auth == "Bearer jwt-1" || auth == "Bearer jwt-2" {
				w.Write([]byte(`{"id":7}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"ghs_minted"}`))
	}), withAssertedMint)
	mint := func(jwt, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/app/installations/9/access_tokens", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	first := mint("jwt-1", `{"repositories":["a"],"permissions":{"contents":"read"}}`)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	rotated := mint("jwt-2", `{"permissions":{"contents":"read"},"repositories":["a"]}`)
	assert.Equal(t, string(OutcomeHit), rotated.Header().Get("X-Mirror-Cache"),
		"the same app asking the same question in another field order is one row")
	assert.EqualValues(t, 1, mints.Load())

	forged := mint("forged", `{}`)
	assert.Equal(t, string(PassNoIdentity), forged.Header().Get("X-Mirror-Passthrough-Reason"),
		"a bearer the upstream does not vouch for is its to answer, uncached")
}

func TestAssert_NeedsAnAssertionRule(t *testing.T) {
	spec := testSpec("https://api.example.com")
	withAssertedMint(spec)
	spec.Identity = nil
	assert.ErrorContains(t, spec.validate(), "assert=")
}

func TestBodyFingerprint_IgnoresFieldOrder(t *testing.T) {
	assert.Equal(t, bodyFingerprint([]byte(`{"a":1,"b":[2]}`)), bodyFingerprint([]byte(`{ "b":[2], "a":1 }`)))
	assert.NotEqual(t, bodyFingerprint([]byte(`{"a":1}`)), bodyFingerprint([]byte(`{"a":2}`)))
	assert.Equal(t, fingerprint("not json"), bodyFingerprint([]byte("not json")))
}
