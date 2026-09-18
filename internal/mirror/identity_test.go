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

func githubIdentity() *Identity {
	return &Identity{
		TTL:       time.Hour,
		User:      &IdentityRule{Path: "/user", Principal: "user:{{ .doc.id }}"},
		Assertion: &IdentityRule{Header: "X-Mirror-Identity", As: "Authorization", Scheme: "Bearer", Path: "/app", Principal: "app:{{ .doc.id }}"},
	}
}

// identityUpstream answers /user and /app by the Authorization it was sent.
func identityUpstream(t *testing.T, calls *atomic.Int32) *identities {
	t.Helper()
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/app" && auth == "Bearer good-jwt":
			w.Write([]byte(`{"id":7,"slug":"minder"}`))
		case r.URL.Path == "/app":
			w.WriteHeader(http.StatusUnauthorized)
		case auth == "token user-a" || auth == "token user-a-rotated":
			w.Write([]byte(`{"id":42,"login":"alice"}`))
		case auth == "token install":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		case auth == "token revoked":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	})
	return newIdentities(githubIdentity(), up, callVars)
}

func asking(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/repos/o/r", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestIdentity_AUserKeepsOnePrincipalAcrossTokens(t *testing.T) {
	var calls atomic.Int32
	ids := identityUpstream(t, &calls)
	ctx := context.Background()

	p, refused := ids.resolve(ctx, asking(map[string]string{"Authorization": "token user-a"}))
	require.Nil(t, refused)
	assert.Equal(t, "user:42", p)
	again, _ := ids.resolve(ctx, asking(map[string]string{"Authorization": "token user-a-rotated"}))
	assert.Equal(t, "user:42", again)

	ids.resolve(ctx, asking(map[string]string{"Authorization": "token user-a"}))
	assert.EqualValues(t, 2, calls.Load(), "a resolved token is remembered for the ttl")
}

func TestIdentity_ATokenThatIsNoUserKeepsItsFingerprint(t *testing.T) {
	var calls atomic.Int32
	ids := identityUpstream(t, &calls)
	p, refused := ids.resolve(context.Background(), asking(map[string]string{"Authorization": "token install"}))
	require.Nil(t, refused)
	assert.Equal(t, "token:"+fingerprint("token install"), p)
}

func TestIdentity_RefusesWhatItCannotResolve(t *testing.T) {
	var calls atomic.Int32
	ids := identityUpstream(t, &calls)
	ctx := context.Background()

	_, refused := ids.resolve(ctx, asking(map[string]string{"Authorization": "token revoked"}))
	require.NotNil(t, refused)
	assert.Equal(t, http.StatusUnauthorized, refused.status)

	_, refused = ids.resolve(ctx, asking(map[string]string{"Authorization": "token upstream-down"}))
	require.NotNil(t, refused)
	assert.Equal(t, http.StatusServiceUnavailable, refused.status, "a guessed principal reveals another caller's grants")
}

func TestIdentity_AnAssertionTheUpstreamVouchesForNamesTheApp(t *testing.T) {
	var calls atomic.Int32
	ids := identityUpstream(t, &calls)
	ctx := context.Background()

	p, refused := ids.resolve(ctx, asking(map[string]string{"Authorization": "token install", "X-Mirror-Identity": "good-jwt"}))
	require.Nil(t, refused)
	assert.Equal(t, "app:7", p)

	_, refused = ids.resolve(ctx, asking(map[string]string{"Authorization": "token install", "X-Mirror-Identity": "forged"}))
	require.NotNil(t, refused)
	assert.Equal(t, http.StatusUnauthorized, refused.status)
}

func TestIdentity_NoBlockKeysByFingerprint(t *testing.T) {
	var ids *identities
	p, refused := ids.resolve(context.Background(), asking(map[string]string{"Authorization": "token x"}))
	require.Nil(t, refused)
	assert.Equal(t, "token:"+fingerprint("token x"), p)
	p, _ = ids.resolve(context.Background(), asking(nil))
	assert.Empty(t, p)
}

func TestIdentity_ValidateRefusesAnUnforwardedCredential(t *testing.T) {
	assert.NoError(t, githubIdentity().validate([]string{"Authorization"}))
	assert.ErrorContains(t, githubIdentity().validate(nil), "does not forward")
	id := githubIdentity()
	id.TTL = 0
	assert.ErrorContains(t, id.validate([]string{"Authorization"}), "ttl")
}
