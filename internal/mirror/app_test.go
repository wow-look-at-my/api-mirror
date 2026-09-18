package mirror

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApp_TheMirrorsOwnCallsCarryTheOwnersInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	var mints atomic.Int32
	var mu sync.Mutex
	sent := map[string]string{}
	seen := func(path string) string {
		mu.Lock()
		defer mu.Unlock()
		return sent[path]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/app/installations":
			requireAppJWT(t, auth, &key.PublicKey)
			w.Write([]byte(`[{"id":5,"account":{"login":"Acme"}}]`))
		case r.URL.Path == "/app/installations/5/access_tokens":
			requireAppJWT(t, auth, &key.PublicKey)
			mints.Add(1)
			body, err := json.Marshal(map[string]any{"token": "ghs_acme", "expires_at": time.Now().Add(time.Hour).UTC()})
			require.NoError(t, err)
			w.Write(body)
		default:
			mu.Lock()
			sent[r.URL.Path] = auth
			mu.Unlock()
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	spec := &Spec{Name: "app", Upstream: Upstream{
		Base:    srv.URL,
		Headers: []Header{{Name: "Authorization", Value: "token fallback", Background: true}},
		Forward: []string{"Authorization"},
		App: &App{ID: "42", Key: pemText, Installations: "/app/installations", Account: "account.login",
			Mint: "/app/installations/{id}/access_tokens", OwnerKey: "owner"},
	}}
	require.NoError(t, spec.Upstream.App.validate())
	up, err := NewUpstreamer(spec, map[string]any{}, nil)
	require.NoError(t, err)
	require.NotNil(t, up.app)

	about := func(owner string) context.Context {
		return withPlan(context.Background(), &fetchPlan{key: map[string]string{"owner": owner}})
	}
	_, err = up.Call(about("acme"), http.MethodGet, "/repos/acme/one", nil, nil, nil)
	require.NoError(t, err)
	_, err = up.Call(about("ACME"), http.MethodGet, "/repos/acme/two", nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "Bearer ghs_acme", seen("/repos/acme/one"))
	assert.Equal(t, "Bearer ghs_acme", seen("/repos/acme/two"))
	assert.EqualValues(t, 1, mints.Load(), "a live token is reused until near its expiry")

	_, err = up.Call(about("stranger"), http.MethodGet, "/repos/stranger/x", nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "token fallback", seen("/repos/stranger/x"), "an owner the App is not installed on keeps the background header")

	_, err = up.Call(withAppCall(context.Background()), http.MethodGet, "/app/hook/deliveries", nil, nil, nil)
	require.NoError(t, err)
	requireAppJWT(t, seen("/app/hook/deliveries"), &key.PublicKey)

	caller := http.Header{}
	caller.Set("Authorization", "token caller")
	_, err = up.Call(about("acme"), http.MethodGet, "/repos/acme/three", nil, caller, nil)
	require.NoError(t, err)
	assert.Equal(t, "token caller", seen("/repos/acme/three"), "a caller's request is sent as that caller and nobody else")
}

func TestApp_UnconfiguredIsNoApp(t *testing.T) {
	app, err := newAppAuth(&App{ID: "", Key: ""}, map[string]any{}, nil)
	require.NoError(t, err)
	assert.Nil(t, app)
	_, err = newAppAuth(&App{ID: "1", Key: "not a key"}, map[string]any{}, nil)
	assert.Error(t, err, "a key that is present and unreadable is loud")
	assert.Error(t, (&App{Installations: "/i", Account: "a", Mint: "/m", OwnerKey: "o"}).validate())
}

// requireAppJWT checks an Authorization value is an RS256 JWT the key signed.
func requireAppJWT(t *testing.T, auth string, pub *rsa.PublicKey) {
	t.Helper()
	token, ok := strings.CutPrefix(auth, "Bearer ")
	require.True(t, ok, auth)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig))
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	require.NoError(t, json.Unmarshal(claims, &c))
	assert.Equal(t, "42", c.Iss)
	assert.Less(t, c.Iat, time.Now().Unix(), "backdated for clock skew")
	assert.LessOrEqual(t, c.Exp-time.Now().Unix(), int64(10*60))
}
