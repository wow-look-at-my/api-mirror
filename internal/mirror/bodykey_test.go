package mirror

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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

func TestBodyKey_ABypassedBodyIsForwardedWholeEveryTime(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	e := bodyKeyEngine(t, &seen, &mu)
	e.spec.Routes[len(e.spec.Routes)-1].Bypass = regexp.MustCompile(`"query"\s*:\s*"[^"]*\bmutation\b`)

	mutation := `{"query":"mutation { addStar(input: {}) { clientMutationId } }"}`
	for range 2 {
		rec := postQuery(t, e, mutation)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "passthrough", rec.Header().Get("X-Mirror-Cache"))
		assert.Equal(t, string(PassBypass), rec.Header().Get("X-Mirror-Passthrough-Reason"))
	}
	query := `{"query":"query { viewer { login } }"}`
	assert.Equal(t, string(OutcomeMiss), postQuery(t, e, query).Header().Get("X-Mirror-Cache"))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{mutation, mutation, query}, seen,
		"every mutation runs upstream with its body intact; a read is still cached")
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

func TestMint_TheScopeTravelsKeysTheTokenAndStaysCreated(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		n := len(seen)
		mu.Unlock()
		answer, err := json.Marshal(map[string]string{"token": "t" + strconv.Itoa(n)})
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write(answer)
	}), func(s *Spec) {
		s.Upstream.Forward = append(s.Upstream.Forward, "Content-Type")
		s.Resources = append(s.Resources, &Resource{
			Name: "install_token", Store: StoreDocument,
			Keys:   []Key{{Name: "token_fp", Credential: true}, {Name: "id"}, {Name: "request"}},
			Reveal: &Reveal{Credential: true},
		})
		s.Routes = append(s.Routes, &Route{
			Method: "POST", Path: "/app/installations/{id}/access_tokens", Resource: "install_token", BodyKey: "request",
		})
	})
	mint := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/app/installations/9/access_tokens", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer app-jwt")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	all := mint(``)
	scoped := mint(`{"repositories":["one"]}`)
	again := mint(`{"repositories":["one"]}`)

	assert.Equal(t, http.StatusCreated, all.Code)
	assert.JSONEq(t, `{"token":"t1"}`, all.Body.String())
	assert.JSONEq(t, `{"token":"t2"}`, scoped.Body.String(),
		"a scoped mint must never be answered with the unscoped token")
	assert.Equal(t, http.StatusCreated, again.Code, "a hit answers the status the upstream gave")
	assert.Equal(t, string(OutcomeHit), again.Header().Get("X-Mirror-Cache"))
	assert.JSONEq(t, `{"token":"t2"}`, again.Body.String())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{``, `{"repositories":["one"]}`}, seen, "the scope reaches the upstream")
}
