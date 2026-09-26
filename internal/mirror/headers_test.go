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

func TestHeaderPrefixRenamesEveryAnswerHeader(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}), func(s *Spec) {
		s.HeaderPrefix = "X-GSM-"
		s.CORS = &CORS{Origins: []string{"*"}}
	})

	r := httptest.NewRequest(http.MethodGet, "/widgets/7", nil)
	r.Header.Set("Origin", "https://viewer.example")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, r)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(OutcomeMiss), rec.Header().Get("X-GSM-Cache"),
		"a consumer of the service this spec replaces reads its own header names")
	assert.Empty(t, rec.Header().Get("X-Mirror-Cache"))
	assert.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "X-GSM-Last-Fetched")
}

func TestFailedRefreshServesTheStoredAnswerAsStale(t *testing.T) {
	var down atomic.Bool
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}), func(s *Spec) {
		s.Resources[0].TTL = time.Nanosecond
	})

	first := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, first.Code)

	down.Store(true)
	time.Sleep(time.Millisecond)
	stale := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, stale.Code, stale.Body.String())
	assert.Equal(t, "true", stale.Header().Get("X-Mirror-Stale"), "a stale answer must say it is stale")
	assert.Equal(t, string(OutcomeStale), stale.Header().Get("X-Mirror-Cache"))
	assert.NotEmpty(t, stale.Header().Get("X-Mirror-Last-Fetched"))
	assert.Contains(t, stale.Body.String(), `"title":"hi"`)
}

func TestFailedFetchWithNothingStoredIsAnError(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	rec := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusBadGateway, rec.Code, "nothing stored means there is nothing to serve stale")
	assert.Empty(t, rec.Header().Get("X-Mirror-Stale"))
}
