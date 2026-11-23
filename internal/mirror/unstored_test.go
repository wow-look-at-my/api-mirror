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

// sharedProof gates both resources on one probe, as routes of one
// repository are.
func sharedProof(s *Spec) {
	for _, res := range s.Resources {
		res.Reveal = &Reveal{Probe: &Probe{Method: http.MethodGet, Path: "/access"}, GrantTTL: time.Hour, DenyTTL: time.Minute}
	}
	s.Upstream.Forward = []string{"Authorization"}
}

// sharedProofUpstream answers the probe, the widget and its parts. The widget
// refuses "token weak". Once removed is set, the probe and the widget refuse
// everyone.
func sharedProofUpstream(probes *atomic.Int32, removed *atomic.Bool) http.Handler {
	refuse := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/access":
			probes.Add(1)
			if removed.Load() {
				refuse(w)
				return
			}
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/parts"):
			w.Write([]byte(`[{"id":"1","name":"bolt"}]`))
		case removed.Load() || r.Header.Get("Authorization") == "token weak":
			refuse(w)
		default:
			w.Write([]byte(`{"id":"7","title":"hello"}`))
		}
	})
}

func TestAFetchRefusalOfOneEndpointKeepsTheProof(t *testing.T) {
	var probes atomic.Int32
	var removed atomic.Bool
	e, _ := newTestEngine(t, sharedProofUpstream(&probes, &removed), sharedProof)

	require.Equal(t, http.StatusForbidden, getAs(t, e, "/widgets/7", "token weak").Code)
	assert.EqualValues(t, 2, probes.Load(), "the probe is asked again, and it still passes")

	parts := getAs(t, e, "/widgets/7/parts", "token weak")
	require.Equal(t, http.StatusOK, parts.Code, parts.Body.String())
	assert.EqualValues(t, 2, probes.Load(), "the proof stands, so the other resource needs no new probe")

	assert.Equal(t, http.StatusForbidden, getAs(t, e, "/widgets/7", "token weak").Code, "the refused key stays refused for the deny window")
}

func TestAFetchRefusalWithARefusedProbeDeniesTheProof(t *testing.T) {
	var probes atomic.Int32
	var removed atomic.Bool
	e, _ := newTestEngine(t, sharedProofUpstream(&probes, &removed), sharedProof)

	require.Equal(t, http.StatusOK, getAs(t, e, "/widgets/7/parts", "token a").Code)
	require.EqualValues(t, 1, probes.Load())

	removed.Store(true)
	require.Equal(t, http.StatusForbidden, getAs(t, e, "/widgets/7", "token a").Code)
	assert.EqualValues(t, 2, probes.Load(), "the refused fetch asks the probe again")

	parts := getAs(t, e, "/widgets/7/parts", "token a")
	assert.Equal(t, http.StatusForbidden, parts.Code, "a refused probe revokes the proof, so a stored row is not shown either")
	assert.EqualValues(t, 2, probes.Load(), "the denial is cached")
}

func TestANotFoundBehindAProbeIsAskedAgain(t *testing.T) {
	var fetches, probes atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/access" {
			probes.Add(1)
			w.Write([]byte(`{}`))
			return
		}
		if fetches.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), sharedProof, func(s *Spec) { s.Routes[0].Absorb = nil })

	require.Equal(t, http.StatusNotFound, getAs(t, e, "/widgets/7", "token a").Code)
	found := getAs(t, e, "/widgets/7", "token a")
	assert.Equal(t, http.StatusOK, found.Code, "a thing that appeared after a 404 is served on the next request: %s", found.Body.String())
	assert.EqualValues(t, 1, probes.Load(), "a 404 asks the probe nothing")
}

func TestAFetchRefusalBelongsToTheCredential(t *testing.T) {
	var probes atomic.Int32
	var removed atomic.Bool
	e, _ := newTestEngine(t, sharedProofUpstream(&probes, &removed), sharedProof)
	ask := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/widgets/7", nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req.WithContext(WithHostPrincipal(req.Context(), "user:1")))
		return rec
	}

	require.Equal(t, http.StatusForbidden, ask("token weak").Code)
	strong := ask("token strong")
	assert.Equal(t, http.StatusOK, strong.Code, "another token of the same owner is asked, and answered: %s", strong.Body.String())
	assert.Equal(t, http.StatusForbidden, ask("token weak").Code)
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
