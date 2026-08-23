package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSpec is a two-resource mirror: one public single-row resource and one
// list. Both are public, so these tests exercise serving rather than the reveal
// ladder, which reveal_test.go covers on its own.
func testSpec(base string) *Spec {
	return &Spec{
		Name:     "test",
		Upstream: Upstream{Base: base},
		Resources: []*Resource{
			{
				Name:  "widget",
				Store: StoreColumns,
				TTL:   time.Hour,
				Keys:  []Key{{Name: "id", From: "id"}},
				Fields: []Field{
					{Name: "title", Type: FieldText, From: "title"},
					{Name: "count", Type: FieldInt, From: "count"},
					{Name: "live", Type: FieldBool, From: "live"},
					{Name: "seen", Type: FieldTime, From: "seen"},
				},
				Drop:   []string{"*url"},
				Reveal: &Reveal{Public: "true"},
			},
			{
				Name:   "part",
				Store:  StoreColumns,
				TTL:    time.Hour,
				Keys:   []Key{{Name: "widget_id"}, {Name: "id", From: "id"}},
				Fields: []Field{{Name: "name", Type: FieldText, From: "name"}},
				Reveal: &Reveal{Public: "true"},
			},
		},
		Routes: []*Route{
			{Method: "GET", Path: "/widgets/{id}", Resource: "widget", Absorb: []int{404}},
			{Method: "GET", Path: "/widgets/{widget_id}/parts", Resource: "part", List: true},
		},
	}
}

// newTestEngine wires a spec to a temp database and an httptest upstream. Each
// tweak edits the spec before it is validated.
func newTestEngine(t *testing.T, h http.Handler, tweaks ...func(*Spec)) (*Engine, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	spec := testSpec(srv.URL)
	for _, tweak := range tweaks {
		tweak(spec)
	}
	require.NoError(t, spec.validate())

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), spec)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	engine, err := NewEngine(spec, store, nil)
	require.NoError(t, err)
	return engine, srv
}

// get drives one request through the engine.
func get(t *testing.T, e *Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServeFetchesThenServesFromCache(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"7","title":"hello","count":3,"live":true,` +
			`"seen":"2026-01-02T03:04:05Z","html_url":"https://upstream/7"}`))
	}))

	first := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, string(OutcomeMiss), first.Header().Get("X-Mirror-Cache"))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &doc))
	assert.Equal(t, "hello", doc["title"])
	assert.EqualValues(t, 3, doc["count"])
	assert.Equal(t, true, doc["live"])
	assert.Equal(t, "2026-01-02T03:04:05Z", doc["seen"])

	// A field the spec never declared is a field the mirror never serves, and
	// a URL key would point a consumer back around the mirror.
	assert.NotContains(t, doc, "html_url")

	second := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, string(OutcomeHit), second.Header().Get("X-Mirror-Cache"))
	assert.JSONEq(t, first.Body.String(), second.Body.String(),
		"hit and miss must rebuild the same answer, or a consumer breaks when the cache is cold")
	assert.EqualValues(t, 1, calls.Load(), "the second read must not reach the upstream")
}

func TestServeListStoresEveryRow(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"a","name":"first"},{"id":"b","name":"second"}]`))
	}))

	rec := get(t, e, "/widgets/7/parts")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
	assert.Equal(t, "first", out[0]["name"])
	// The list route's own key pins the rows it owns, so an item that does not
	// carry it is still filed where the list will find it again.
	assert.Equal(t, "7", out[0]["widget_id"])
}

func TestUndeclaredPathPassesThroughWithAReason(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"upstream":"answered"}`))
	}))

	rec := get(t, e, "/something/else")
	assert.Equal(t, "passthrough", rec.Header().Get("X-Mirror-Cache"))
	assert.Equal(t, string(PassUnrouted), rec.Header().Get("X-Mirror-Passthrough-Reason"))
	assert.Contains(t, rec.Body.String(), "answered")
}

func TestDeclaredPathWithAnotherMethodPassesThrough(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/widgets/7", nil))
	assert.Equal(t, string(PassMethod), rec.Header().Get("X-Mirror-Passthrough-Reason"))
}

func TestTransientUpstreamFailureIsNotStored(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"id":"7","title":"recovered"}`))
	}))

	first := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusBadGateway, first.Code)

	// The failure is remembered only for its backoff window, and the window is
	// about not hammering a failing upstream. Recording it as the ANSWER would
	// serve an outage long after it ended, so nothing was stored.
	require.NoError(t, clearBackoff(e, "widget", "7"))

	second := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Contains(t, second.Body.String(), "recovered")
}

// clearBackoff drops a key's freshness row, which is what a deliberate refresh
// does. It stands in for waiting out the retry window.
func clearBackoff(e *Engine, kind, key string) error {
	res, _ := e.store.Resource(kind)
	_, err := e.store.DeleteFreshness(context.Background(), kind, keyString(res, map[string]string{"id": key}))
	return err
}

func TestBackoffReplaysTheStoredFailure(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	assert.Equal(t, http.StatusBadGateway, get(t, e, "/widgets/7").Code)
	assert.Equal(t, http.StatusBadGateway, get(t, e, "/widgets/7").Code)
	assert.EqualValues(t, 1, calls.Load(),
		"a failing upstream is asked once per window, not once per request")
}

func TestAbsorbedNotFoundIsRemembered(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{}`))
	}))

	first := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusNotFound, first.Code)

	second := get(t, e, "/widgets/7")
	assert.Equal(t, http.StatusNotFound, second.Code)
	assert.EqualValues(t, 1, calls.Load(),
		"the route names 404 absorbable, so the upstream states it once")
}

func TestUnmodelledQueryPassesThrough(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"forwarded":true}`))
	}), func(s *Spec) {
		s.Routes[0].Query = []QueryParam{{Name: "fields", Type: FieldText, Default: "all"}}
	})

	rec := get(t, e, "/widgets/7?nope=1")
	assert.Equal(t, string(PassQuery), rec.Header().Get("X-Mirror-Passthrough-Reason"),
		"a row keyed on a shape the spec never described would answer a different question")
	assert.Contains(t, rec.Body.String(), "forwarded")
}

func TestQueryValueOutsideItsRangePassesThrough(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"forwarded":true}`))
	}), func(s *Spec) {
		s.Routes[0].Query = []QueryParam{
			{Name: "page", Type: FieldInt, Default: "1", Min: 1, Max: 10},
		}
	})

	rec := get(t, e, "/widgets/7?page=99")
	assert.Equal(t, string(PassQuery), rec.Header().Get("X-Mirror-Passthrough-Reason"),
		"clamping would answer a question the caller did not ask")
}

func TestAcceptOutsideTheModelPassesThrough(t *testing.T) {
	rt := &Route{Path: "/x", Accept: []string{"application/json"}}
	assert.True(t, acceptable(rt, ""), "no Accept means the caller takes anything")
	assert.True(t, acceptable(rt, "*/*"))
	assert.True(t, acceptable(rt, "application/json"))
	assert.False(t, acceptable(rt, "application/vnd.github.diff"),
		"a mirror rebuilds JSON; it cannot rebuild a diff")
}

func TestMatchPath(t *testing.T) {
	params, ok := matchPath("/repos/{owner}/{repo}/pulls", "/repos/acme/tools/pulls")
	require.True(t, ok)
	assert.Equal(t, map[string]string{"owner": "acme", "repo": "tools"}, params)

	_, ok = matchPath("/repos/{owner}", "/repos/acme/tools")
	assert.False(t, ok)

	_, ok = matchPath("/repos/{owner}", "/repos/")
	assert.False(t, ok, "an empty segment does not bind a parameter")
}

func TestKeyFoldingKeepsOneRowPerFact(t *testing.T) {
	spec := testSpec("http://example.invalid")
	spec.Resources[0].Keys = []Key{{Name: "id", From: "id", Fold: true}}
	require.NoError(t, spec.validate())
	assert.Equal(t, "abc", foldFor(spec.Resources[0], "id", "ABC"),
		"a differently cased URL must not land on a row a delivery never reaches")
}
