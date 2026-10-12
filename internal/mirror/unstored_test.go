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

// probedWidgets gates the widget resource on a probe, so a caller has to
// prove access before a read.
func probedWidgets(s *Spec) {
	s.Resources[0].Reveal = &Reveal{
		Probe:    &Probe{Method: http.MethodGet, Path: "/probe/{id}"},
		GrantTTL: time.Hour,
		DenyTTL:  time.Minute,
	}
	s.Upstream.Forward = []string{"Authorization"}
}

func getAs(t *testing.T, e *Engine, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestAFetchRefusalIsTheCallersDenial(t *testing.T) {
	var fetches atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/probe/") {
			w.Write([]byte(`{}`))
			return
		}
		fetches.Add(1)
		if r.Header.Get("Authorization") == "token weak" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), probedWidgets)

	refused := getAs(t, e, "/widgets/7", "token weak")
	require.Equal(t, http.StatusForbidden, refused.Code)
	assert.Contains(t, refused.Body.String(), "not accessible",
		"the caller gets the upstream's own refusal, not a gateway error")

	again := getAs(t, e, "/widgets/7", "token weak")
	assert.Equal(t, http.StatusForbidden, again.Code)
	assert.EqualValues(t, 1, fetches.Load(), "the refusal is a cached denial, so the caller does not re-ask")

	ok := getAs(t, e, "/widgets/7", "token strong")
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, string(OutcomeMiss), ok.Header().Get("X-Mirror-Cache"),
		"one caller's refusal is not a backoff another caller reads")
}

func TestABadCredentialIsTold401(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Bad credentials"))
	}), probedWidgets)

	rec := getAs(t, e, "/widgets/7", "token expired")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a bad credential is the caller's problem, not a gateway failure")
}

func TestAnUnmodelledAnswerIsRelayedAndNotStored(t *testing.T) {
	var fetches atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"No commit found"}`))
	}))

	first := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusUnprocessableEntity, first.Code)
	assert.Equal(t, string(PassResponse), first.Header().Get("X-Mirror-Passthrough-Reason"))
	assert.Contains(t, first.Body.String(), "No commit found")

	second := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusUnprocessableEntity, second.Code, "an unmodelled answer earns no backoff")
	assert.EqualValues(t, 2, fetches.Load())
}
