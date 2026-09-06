package mirror

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyKeyEngine serves a POST route keyed by its request body, the shape a
// query language needs: every question shares a path.
func bodyKeyEngine(t *testing.T, seen *[]string, mu *sync.Mutex) *Engine {
	t.Helper()
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, string(body))
		mu.Unlock()
		echo, err := json.Marshal(map[string]json.RawMessage{"answer": json.RawMessage(body)})
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.Write(echo)
	}), func(s *Spec) {
		s.Upstream.Forward = append(s.Upstream.Forward, "Content-Type")
		s.Resources = append(s.Resources, &Resource{
			Name: "query", Store: StoreDocument,
			Keys:   []Key{{Name: "token_fp", Credential: true}, {Name: "q"}},
			Reveal: &Reveal{Credential: true},
		})
		s.Routes = append(s.Routes, &Route{
			Method: "POST", Path: "/graphql", Resource: "query", BodyKey: "q",
		})
	})
	return e
}

func postQuery(t *testing.T, e *Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer a-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestBodyKey_TheQuestionTravelsAndKeysTheAnswer(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	e := bodyKeyEngine(t, &seen, &mu)

	first := postQuery(t, e, `1`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, string(OutcomeMiss), first.Header().Get("X-Mirror-Cache"))
	assert.JSONEq(t, `{"answer":1}`, first.Body.String())

	repeat := postQuery(t, e, `1`)
	require.Equal(t, http.StatusOK, repeat.Code)
	assert.Equal(t, string(OutcomeHit), repeat.Header().Get("X-Mirror-Cache"),
		"the same question is the same key, so it is a hit")
	assert.Equal(t, first.Body.String(), repeat.Body.String(),
		"hit and miss answer the same bytes")

	other := postQuery(t, e, `2`)
	require.Equal(t, http.StatusOK, other.Code)
	assert.Equal(t, string(OutcomeMiss), other.Header().Get("X-Mirror-Cache"),
		"a different question must never read the first one's row")
	assert.JSONEq(t, `{"answer":2}`, other.Body.String())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{`1`, `2`}, seen,
		"the body reaches the upstream verbatim, and the repeat costs no call at all")
}

func TestValidateRoute_BodyKey(t *testing.T) {
	resources := map[string]*Resource{"query": {
		Name: "query", Store: StoreDocument,
		Keys:   []Key{{Name: "token_fp", Credential: true}, {Name: "q"}},
		Reveal: &Reveal{Credential: true},
	}}

	t.Run("a POST keyed by its body passes", func(t *testing.T) {
		rt := &Route{Method: "POST", Path: "/graphql", Resource: "query", BodyKey: "q"}
		require.NoError(t, rt.validate(resources))
	})

	t.Run("a read sends no body", func(t *testing.T) {
		rt := &Route{Method: "GET", Path: "/graphql", Resource: "query", BodyKey: "q"}
		require.Error(t, rt.validate(resources))
	})

	t.Run("the named key must exist", func(t *testing.T) {
		rt := &Route{Method: "POST", Path: "/graphql", Resource: "query", BodyKey: "nope"}
		require.Error(t, rt.validate(resources))
	})
}
