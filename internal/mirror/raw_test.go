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

const diffMedia = "application/vnd.github.diff"

// withDiff adds a raw resource answering the widget path in diff form.
func withDiff(s *Spec) {
	s.Routes[0].Accept = []string{"application/json"}
	s.Resources = append(s.Resources, &Resource{
		Name:   "widget_diff",
		Store:  StoreRaw,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "id"}},
		Reveal: &Reveal{Public: "true"},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "GET", Path: "/widgets/{id}", Resource: "widget_diff",
		Accept: []string{diffMedia}, Absorb: []int{406},
	})
}

func getAs(t *testing.T, e *Engine, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	e.ServeHTTP(rec, req)
	return rec
}

func TestRaw_ServesTheBodyByteForByteInItsMediaType(t *testing.T) {
	const diff = "diff --git a/x b/x\n+added\n"
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") != diffMedia {
			w.Write([]byte(`{"id":"7","title":"json"}`))
			return
		}
		w.Header().Set("Content-Type", diffMedia)
		w.Write([]byte(diff))
	}), withDiff)

	first := getAs(t, e, "/widgets/7", diffMedia)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, diff, first.Body.String())
	assert.Equal(t, diffMedia, first.Header().Get("Content-Type"))

	second := getAs(t, e, "/widgets/7", diffMedia)
	assert.Equal(t, string(OutcomeHit), second.Header().Get("X-Mirror-Cache"))
	assert.Equal(t, diff, second.Body.String())

	asJSON := getAs(t, e, "/widgets/7", "application/json")
	assert.Contains(t, asJSON.Body.String(), `"title":"json"`, "the JSON route on the same path still answers JSON")
	assert.EqualValues(t, 2, calls.Load())

	patch := getAs(t, e, "/widgets/7", "application/vnd.github.patch")
	assert.Equal(t, string(PassAccept), patch.Header().Get("X-Mirror-Passthrough-Reason"))
}

func TestRaw_KeepsTheDeclaredRefusal(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotAcceptable)
		w.Write([]byte(`{"message":"Sorry, the diff exceeded the maximum number of lines"}`))
	}), withDiff)

	assert.Equal(t, http.StatusNotAcceptable, getAs(t, e, "/widgets/7", diffMedia).Code)
	assert.Equal(t, http.StatusNotAcceptable, getAs(t, e, "/widgets/7", diffMedia).Code)
	assert.EqualValues(t, 1, calls.Load(), "a stored 406 is answered without asking again")
}

func TestRaw_RouteNeedsAnAcceptAndIsNeverAList(t *testing.T) {
	spec := testSpec("https://api.example.com")
	withDiff(spec)
	spec.Routes[2].Accept = nil
	assert.ErrorContains(t, spec.validate(), "raw resource")
}
