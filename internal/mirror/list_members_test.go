package mirror

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func names(t *testing.T, body []byte) []string {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	got := make([]string, len(out))
	for i, item := range out {
		got[i], _ = item["name"].(string)
	}
	return got
}

// A warm list must answer what the upstream's page held. A scan of every row
// under the parent key also serves rows other routes stored.
func TestListAnswersOnlyTheRowsItsPageHeld(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.URL.Path == "/widgets/7/parts" && r.URL.Query().Get("page") == "2":
			w.Write([]byte(`[{"id":"c","name":"third"}]`))
		case r.URL.Path == "/widgets/7/parts":
			w.Write([]byte(`[{"id":"b","name":"second"},{"id":"a","name":"first"}]`))
		case r.URL.Path == "/widgets/7/parts/z":
			w.Write([]byte(`{"id":"z","name":"elsewhere"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}), func(s *Spec) {
		s.Routes[1].Query = []QueryParam{{Name: "page", Type: FieldInt, Default: "1", Min: 1, Max: 10}}
		s.Routes = append(s.Routes, &Route{Method: "GET", Path: "/widgets/{widget_id}/parts/{id}", Resource: "part"})
	})

	cold := get(t, e, "/widgets/7/parts")
	require.Equal(t, http.StatusOK, cold.Code, cold.Body.String())
	assert.Equal(t, []string{"second", "first"}, names(t, cold.Body.Bytes()), "the upstream's order, not the key's")

	require.Equal(t, http.StatusOK, get(t, e, "/widgets/7/parts/z").Code)

	warm := get(t, e, "/widgets/7/parts")
	assert.Equal(t, string(OutcomeHit), warm.Header().Get("X-Mirror-Cache"))
	assert.Equal(t, []string{"second", "first"}, names(t, warm.Body.Bytes()),
		"a row the single route stored is not on the page the upstream answered")

	page2 := get(t, e, "/widgets/7/parts?page=2")
	assert.Equal(t, string(OutcomeMiss), page2.Header().Get("X-Mirror-Cache"), "page 2 is its own answer")
	assert.Equal(t, []string{"third"}, names(t, page2.Body.Bytes()))
	assert.EqualValues(t, 3, calls.Load())
}

func TestPlanForReadsTheListQueryBack(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Routes[1].Query = []QueryParam{{Name: "page", Type: FieldInt, Default: "1", Min: 1, Max: 10}}
	})
	res, _ := e.store.Resource("part")
	rt := e.spec.Routes[1]
	key := answerKey(res, rt, map[string]string{"widget_id": "7"}, map[string]string{"page": "2"})

	plan, ok := e.planFor(routeKind(rt), StaleKey{Key: key})
	require.True(t, ok)
	assert.Equal(t, "2", plan.query["page"], "a sweep refetches the page it holds, not page 1")
	assert.NotContains(t, plan.key, queryExtra+"page", "the query is not a row key")
	assert.Equal(t, "/widgets/7/parts?page=2", plan.upstreamPath())
}
