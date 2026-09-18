package mirror

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveRates_AskEachInstallationWithItsOwnToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	reset := time.Now().Add(time.Hour).Unix()
	write := func(w http.ResponseWriter, v any) {
		body, err := json.Marshal(v)
		require.NoError(t, err)
		w.Write(body)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			write(w, []map[string]any{{"id": 5, "account": map[string]any{"login": "Acme"}}, {"id": 6, "account": map[string]any{"login": "Beta"}}})
		case "/app/installations/5/access_tokens":
			write(w, map[string]any{"token": "ghs_acme", "expires_at": time.Now().Add(time.Hour).UTC()})
		case "/app/installations/6/access_tokens":
			http.Error(w, "suspended", http.StatusForbidden)
		case "/rate_limit":
			require.Equal(t, "Bearer ghs_acme", r.Header.Get("Authorization"))
			write(w, map[string]any{"resources": map[string]any{
				"core":    map[string]any{"limit": 5000, "remaining": 4990, "used": 10, "reset": reset},
				"graphql": map[string]any{"limit": 5000, "remaining": 5000, "used": 0, "reset": reset},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	spec := &Spec{Name: "rates", Upstream: Upstream{
		Base:    srv.URL,
		Forward: []string{"Authorization"},
		Rate:    RateHeaders{Poll: "/rate_limit", PollField: "resources"},
		App: &App{ID: "42", Key: pemText, Installations: "/app/installations", Account: "account.login",
			Mint: "/app/installations/{id}/access_tokens", OwnerKey: "owner"},
	}}
	up, err := NewUpstreamer(spec, map[string]any{}, nil)
	require.NoError(t, err)
	e := &Engine{spec: spec, up: up, vars: map[string]any{}}

	live, note := e.liveRates(context.Background())
	assert.Empty(t, note)
	require.Len(t, live, 2)
	assert.Equal(t, "acme", live[0].Account)
	require.Len(t, live[0].Resources, 2)
	assert.Equal(t, "core", live[0].Resources[0].Resource)
	assert.Equal(t, 4990, live[0].Resources[0].Remaining)
	assert.Equal(t, time.Unix(reset, 0), live[0].Resources[0].Reset)
	assert.Equal(t, "beta", live[1].Account)
	assert.NotEmpty(t, live[1].Error, "an installation that cannot mint says so instead of vanishing")
}

func TestLiveRates_WithoutAnAppSaysWhy(t *testing.T) {
	e := &Engine{spec: &Spec{Upstream: Upstream{Rate: RateHeaders{Poll: "/rate_limit", PollField: "resources"}}}, up: &Upstreamer{}}
	live, note := e.liveRates(context.Background())
	assert.Nil(t, live)
	assert.Contains(t, note, "no App configured")
}

func TestIdentityNames_ComeFromLiveVerdictsOnly(t *testing.T) {
	now := time.Now()
	id := &identities{now: func() time.Time { return now }, cache: map[string]identityVerdict{
		"user:a": {principal: "user:1", name: "octo", expires: now.Add(time.Hour)},
		"user:b": {principal: "user:2", name: "gone", expires: now.Add(-time.Second)},
		"user:c": {principal: "user:3", expires: now.Add(time.Hour)},
	}}
	assert.Equal(t, map[string]string{"user:1": "octo"}, id.names())
}
