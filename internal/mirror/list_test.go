package mirror

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A list answer that stops mentioning an item is the upstream saying the item
// is gone. Whether the mirror may act on that is the route's declaration, not a
// guess: page of a paginated list is not the set.
func TestCompleteListDropsWhatVanishedUpstream(t *testing.T) {
	var round atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round.Add(1) == 1 {
			w.Write([]byte(`[{"id":"a","name":"first"},{"id":"b","name":"second"}]`))
			return
		}
		w.Write([]byte(`[{"id":"a","name":"first"}]`))
	}), func(s *Spec) {
		s.Routes[1].Complete = true
		s.Routes[1].TTL = 1 // expire at, so the read refetches
	})

	require.Len(t, listNames(t, e), 2)
	assert.Equal(t, []string{"first"}, listNames(t, e),
		"b is gone upstream and the route says the answer is the whole set")
}

func TestIncompleteListKeepsWhatItNoLongerMentions(t *testing.T) {
	var round atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if round.Add(1) == 1 {
			w.Write([]byte(`[{"id":"a","name":"first"},{"id":"b","name":"second"}]`))
			return
		}
		w.Write([]byte(`[{"id":"a","name":"first"}]`))
	}), func(s *Spec) {
		s.Routes[1].TTL = 1
	})

	require.Len(t, listNames(t, e), 2)
	assert.Len(t, listNames(t, e), 2,
		"an undeclared list may be one page, so dropping the rest would lose them")
}

func TestReplaceRefusesAnEmptyKey(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	res, ok := e.store.Resource("part")
	require.True(t, ok)

	err := e.store.ReplaceMany(t.Context(), res, map[string]string{}, nil, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would empty the table",
		"a replace with no key is a truncate wearing a cache update's clothes")
}

// A <key> that states no path reads the field of its own name. Without that a
// list route absorbs nothing: every item is refused for a key nobody supplied,
// and the spec author sees an empty answer with no hint why.
func TestKeyWithoutAPathReadsItsOwnName(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"a","name":"first"}]`))
	}), func(s *Spec) {
		// The parts resource keys on id; drop the explicit path it was given.
		for i := range s.Resources[1].Keys {
			s.Resources[1].Keys[i].From = ""
		}
	})

	assert.Equal(t, []string{"first"}, listNames(t, e))
}

// listNames reads the list route and returns each row's name.
func listNames(t *testing.T, e *Engine) []string {
	t.Helper()
	rec := get(t, e, "/widgets/7/parts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	names := make([]string, 0, len(out))
	for _, row := range out {
		names = append(names, row["name"].(string))
	}
	return names
}

func TestDocumentListAnswersThePageItStored(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"sha":"a"},{"sha":"b"}]`))
	}), func(s *Spec) {
		s.Resources = append(s.Resources, &Resource{
			Name: "commit_page", Store: StoreDocument, TTL: time.Hour,
			Keys:   []Key{{Name: "id"}, {Name: "page"}},
			Reveal: &Reveal{Public: "true"},
		})
		s.Routes = append(s.Routes, &Route{
			Method: "GET", Path: "/widgets/{id}/commits", Resource: "commit_page", List: true,
			Query: []QueryParam{{Name: "page", Type: FieldInt, Default: "1", Min: 1, Max: 100, Key: true}},
		})
	})

	for _, want := range []string{string(OutcomeMiss), string(OutcomeHit)} {
		rec := get(t, e, "/widgets/7/commits")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, want, rec.Header().Get("X-Mirror-Cache"))
		assert.JSONEq(t, `[{"sha":"a"},{"sha":"b"}]`, rec.Body.String(),
			"the stored page is the answer, never wrapped in a second array")
	}
}

func TestValidateRoute_DocumentListNeedsTheWholeKey(t *testing.T) {
	res := &Resource{Name: "page", Store: StoreDocument, Keys: []Key{{Name: "id"}, {Name: "page"}},
		Reveal: &Reveal{Public: "true"}}
	rt := &Route{Method: "GET", Path: "/widgets/{id}/commits", Resource: "page", List: true}
	err := rt.validate(map[string]*Resource{"page": res})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `nothing supplies "page"`)
}
