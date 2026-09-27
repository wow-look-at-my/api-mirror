package mirror

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(block)
}

func TestJWTIsASignedAppAssertion(t *testing.T) {
	key, pemText := testKeyPEM(t)
	// An environment variable often carries the PEM with escaped newlines.
	token, err := renderString(`{{ jwt "12345" .key }}`, map[string]any{"key": strings.ReplaceAll(pemText, "\n", `\n`)})
	require.NoError(t, err)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(claimsJSON, &claims))
	assert.Equal(t, "12345", claims["iss"])
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	assert.WithinDuration(t, time.Now().Add(jwtLifetime), exp, 5*time.Second)
	assert.Less(t, claims["iat"].(float64), float64(time.Now().Unix()), "backdated for clock skew")

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	assert.NoError(t, rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig))

	again, err := renderString(`{{ jwt "12345" .key }}`, map[string]any{"key": pemText})
	require.NoError(t, err)
	assert.Equal(t, token, again, "a live token is reused, not re-signed per call")
}

func TestJWTRefusesWhatIsNotAKey(t *testing.T) {
	_, err := renderString(`{{ jwt "1" "not a key" }}`, nil)
	assert.Error(t, err)
	_, err = renderString(`{{ jwt "" "x" }}`, nil)
	assert.Error(t, err)
}

func TestReadFileReadsAMountedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(path, []byte("pem"), 0o600))
	out, err := renderString(`{{ readFile .path }}`, map[string]any{"path": path})
	require.NoError(t, err)
	assert.Equal(t, "pem", out)
	out, err = renderString(`{{ readFile "" }}`, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestReplayAuthenticatesWithItsOwnHeaders(t *testing.T) {
	var auth atomic.Value
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`[]`))
	}), func(s *Spec) {
		s.Upstream.Headers = append(s.Upstream.Headers, Header{Name: "Authorization", Value: "token background"})
		s.Replay = &Replay{
			Interval: time.Minute, List: "/deliveries", Redeliver: "/deliveries/{{ .id }}", ID: "id",
			Headers: []Header{{Name: "Authorization", Value: "Bearer app-jwt"}},
		}
	})
	e.replay.cycle()
	require.EqualValues(t, 1, calls.Load())
	assert.Equal(t, "Bearer app-jwt", auth.Load(), "the replayer's header replaces the sweep's credential")
}
