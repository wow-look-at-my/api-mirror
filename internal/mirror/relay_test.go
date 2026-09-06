package mirror

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An API's login endpoints sit on a different host from its data and send no
// CORS headers, so a browser app cannot complete a sign-in against them. A
// relay forwards the body and puts the mirror's own CORS policy in front.
func TestRelay_ForwardsTheBodyToAForeignHost(t *testing.T) {
	var got atomic.Value
	var sawAuth atomic.Bool
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Store(string(body))
		sawAuth.Store(r.Header.Get("Authorization") != "")
		w.Header().Set("Content-Type", "application/json")
		// The foreign host sets its own allow-origin, which must not survive.
		w.Header().Set("Access-Control-Allow-Origin", "https://github.com")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"access_token":"t"}`))
	}))
	t.Cleanup(foreign.Close)

	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the upstream must never see a relayed path, got %s", r.URL.Path)
	}), func(s *Spec) {
		s.Relays = append(s.Relays, &Relay{Method: "POST", Path: "/login/oauth/access_token", To: foreign.URL})
	})

	req := httptest.NewRequest(http.MethodPost, "/login/oauth/access_token",
		strings.NewReader(`{"client_secret":"s"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer should-not-travel")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"access_token":"t"}`, rec.Body.String())
	assert.Equal(t, `{"client_secret":"s"}`, got.Load(), "the body is forwarded verbatim")
	assert.False(t, sawAuth.Load(),
		"the body is the credential here, so the caller's bearer must not reach a foreign host")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"the mirror is the single CORS authority, and a duplicate allow-origin breaks the browser")
	assert.Equal(t, string(PassRelay), rec.Header().Get("X-Mirror-Passthrough-Reason"),
		"a relay is uncached traffic and says so, so it shows up on the chart")
}

func TestValidateRelay(t *testing.T) {
	ok := func() *Relay {
		return &Relay{Method: "POST", Path: "/login/device/code", To: "https://github.com/login/device/code"}
	}
	require.NoError(t, ok().validate())

	for name, break_ := range map[string]func(*Relay){
		"a relative path":           func(r *Relay) { r.Path = "login" },
		"no method":                 func(r *Relay) { r.Method = "" },
		"a parameter in a path":     func(r *Relay) { r.Path = "/login/{id}" },
		"a relative to":             func(r *Relay) { r.To = "/login" },
		"plaintext off the machine": func(r *Relay) { r.To = "http://github.com/login" },
	} {
		t.Run(name, func(t *testing.T) {
			rl := ok()
			break_(rl)
			require.Error(t, rl.validate())
		})
	}
}
