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

// privateWidgets makes the widget private and names callers through /user,
// so a read needs a principal and a probe.
func privateWidgets(s *Spec) {
	s.Upstream.Forward = append(s.Upstream.Forward, "Authorization")
	s.Identity = githubIdentity()
	s.Resources[0].Reveal = &Reveal{
		Public: "false", Probe: &Probe{Method: http.MethodGet, Path: "/widgets/{{ .key.id }}"},
		GrantTTL: time.Hour, DenyTTL: time.Minute,
	}
}

func TestAHostPrincipalReplacesTheIdentityLookup(t *testing.T) {
	var lookups, probes atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			lookups.Add(1)
			w.Write([]byte(`{"id":42}`))
			return
		}
		probes.Add(1)
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), privateWidgets)
	ask := func(token, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/widgets/7", nil)
		req.Header.Set("Authorization", "token "+token)
		if host != "" {
			req = req.WithContext(WithHostPrincipal(req.Context(), host))
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	first := ask("a", "installation:9")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Zero(t, lookups.Load(), "the host named the caller, so the engine asks nobody")
	asked := probes.Load()

	rotated := ask("b", "installation:9")
	require.Equal(t, http.StatusOK, rotated.Code)
	assert.Equal(t, asked, probes.Load(), "another token of the same host principal reads under the same grant")
	assert.Zero(t, lookups.Load())

	bare := ask("c", "")
	require.Equal(t, http.StatusOK, bare.Code)
	assert.Positive(t, lookups.Load(), "without a host principal the spec's rule resolves the caller")
	assert.Greater(t, probes.Load(), asked, "a different principal earns its own grant")
}
