package mirror

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rule these tests hold: everything the mirror exchanges is visible. A
// request nobody can see is a request nobody can account for -- it spends the
// budget, it can be slow, it can fail, and none of that reaches the operator.

func TestObservedClient_ReportsFromTheTransportSoNoCallSiteCanSkipIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`hello`))
	}))
	t.Cleanup(srv.Close)

	tel := NewTelemetry(RateHeaders{})
	client := observedClient(&http.Client{}, LaneProbe, tel)

	// Deliberately a bare Do, with nothing instrumenting the call site. The
	// point is that the report happens anyway.
	resp, err := client.Get(srv.URL + "/thing")
	require.NoError(t, err)
	_, _, err = readCapped(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	frames := tel.Timeline.Frames()
	require.Len(t, frames, 1, "an uninstrumented call site still reports, because the transport does it")
	assert.Equal(t, LaneProbe, frames[0].Lane)
	assert.Equal(t, "/thing", frames[0].Path)
	assert.Equal(t, http.StatusOK, frames[0].Status)
	assert.Equal(t, len("hello"), frames[0].Bytes,
		"the byte count is taken when the body is done, so it is what the exchange actually cost")
}

func TestObservedClient_ReportsARequestThatNeverArrived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()

	tel := NewTelemetry(RateHeaders{})
	client := observedClient(&http.Client{Timeout: 2 * time.Second}, LaneFetch, tel)

	_, err := client.Get(base + "/gone")
	require.Error(t, err)

	frames := tel.Timeline.Frames()
	require.Len(t, frames, 1, "a failed request costs time and must not vanish from the chart")
	assert.NotEmpty(t, frames[0].Error)
	assert.Zero(t, frames[0].Status)
}

func TestObservedClient_ReadsTheBudgetFromTheHeadersNotTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4998")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	tel := NewTelemetry(RateHeaders{
		Limit:     "X-RateLimit-Limit",
		Remaining: "X-RateLimit-Remaining",
		Reset:     "X-RateLimit-Reset",
	})
	client := observedClient(&http.Client{}, LaneFetch, tel)
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req = req.WithContext(withLane(req.Context(), LaneFetch, "token:abc", ""))

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	budgets := tel.Rates.Snapshot()
	require.Len(t, budgets, 1)
	assert.Equal(t, "token:abc", budgets[0].Principal)
	assert.Equal(t, 4998, budgets[0].Remaining)
	assert.Equal(t, 5000, budgets[0].Limit)
	assert.False(t, budgets[0].Stale)
}

func TestRateMeter_SaysAPastResetIsPastRatherThanImminent(t *testing.T) {
	m := NewRateMeter(RateHeaders{Limit: "L", Remaining: "R", Reset: "X"})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	past := now.Add(-30 * time.Minute)
	m.Observe("token:abc", http.Header{
		"L": {"100"}, "R": {"7"}, "X": {itoa(int(past.Unix()))},
	})

	got := m.Snapshot()
	require.Len(t, got, 1)
	assert.True(t, got[0].Stale,
		"a reset already behind us describes a window that is over; rendering it as 'resets now' is a lie the page would repeat")
}

func TestRateMeter_DropsAnIdentityThatStoppedCalling(t *testing.T) {
	m := NewRateMeter(RateHeaders{Limit: "L", Remaining: "R", Reset: "X"})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	m.Observe("token:gone", http.Header{"L": {"100"}, "R": {"7"}, "X": {itoa(int(now.Add(time.Minute).Unix()))}})
	require.Len(t, m.Snapshot(), 1)

	// An identity still calling is re-observed with a fresh reset on every
	// answer, so a reset well in the past means that identity stopped.
	now = now.Add(3 * time.Hour)
	assert.Empty(t, m.Snapshot(), "a dead reading is swept lazily, with no goroutine whose only job is to delete")
}

func TestTimeline_ReportsWhatItDroppedRatherThanLosingItQuietly(t *testing.T) {
	tl := NewTimeline()
	// Shrink the ring so the eviction path is reachable in a test rather than
	// only after a hundred thousand requests.
	tl.frames = make([]Frame, 2)

	for i := range 5 {
		tl.Observe(Exchange{Lane: LaneFetch, Method: "GET", Path: "/" + itoa(i), Started: time.Now()})
	}
	stats := tl.Stats()
	assert.Equal(t, 2, stats.Frames)
	assert.Equal(t, 3, stats.Dropped,
		"a chart that silently loses its oldest frames reads as a quiet period that never happened")
}

func TestRequestLog_TalliesByShapeSoOneOffPathsDoNotHideTheFamily(t *testing.T) {
	log := NewRequestLog()
	for _, path := range []string{"/repos/a/b", "/repos/c/d", "/repos/e/f"} {
		log.Record(Request{
			Method: "GET", Path: path, Shape: "/repos/{owner}/{repo}",
			Disposition: DispPassthrough, Reason: "unrouted", Duration: time.Millisecond,
		})
	}
	groups := log.Groups()
	require.Len(t, groups, 1, "three concrete paths of one family are one row, or the table is a list of one-offs")
	assert.Equal(t, 3, groups[0].Count)
	assert.Equal(t, 3, groups[0].Dispositions[DispPassthrough])
	assert.Equal(t, 3, groups[0].Reasons["unrouted"])
	assert.Len(t, groups[0].Samples, 3, "the samples let an author check the guess the shape made")
	assert.Equal(t, time.Millisecond, groups[0].MeanDuration())
}

func TestGeneralizeWith_UsesTheSpecsOwnWordsRatherThanGuessing(t *testing.T) {
	vocab := pathVocabulary(&Spec{Routes: []*Route{
		{Path: "/repos/{owner}/{repo}/pulls/{number}"},
	}})
	// "octocat" and "pulls" are indistinguishable to a shape heuristic and mean
	// opposite things. The declared vocabulary is what tells them apart.
	assert.Equal(t, "/repos/{id}/{id}/releases", generalizeWith(vocab, "/repos/octocat/hello/releases"))
	assert.Equal(t, "/repos/{id}/{id}/pulls/{id}", generalizeWith(vocab, "/repos/octocat/hello/pulls/42"))
}

func TestSketchRoute_NamesEachParameterAfterWhatPrecedesIt(t *testing.T) {
	sketch := sketchRoute("GET", "/repos/{id}/{id}/pulls/{id}")
	assert.Contains(t, sketch, `<key name="repo"`)
	assert.Contains(t, sketch, `<key name="pull"`)
	assert.Contains(t, sketch, `path="/repos/{repo}/{repo2}/pulls/{pull}"`,
		"three parameters all called {id} would be a declaration that cannot load")
}

func TestParseKeyString_IsTheInverseOfKeyString(t *testing.T) {
	res := &Resource{
		Name: "pull",
		Keys: []Key{{Name: "owner"}, {Name: "repo"}, {Name: "number"}},
	}
	key := map[string]string{"owner": "octo cat", "repo": "hello/world", "number": "42", "state": "open"}

	round, ok := parseKeyString(res, keyString(res, key))
	require.True(t, ok)
	assert.Equal(t, key, round,
		"the sweep decodes what a read encoded; disagreeing refreshes a row nobody reads and leaves the one they do")
}

func TestAssets_AreAllEmbeddedAndAllReferenced(t *testing.T) {
	// A missing embed is a compile error. A <script src> that no longer matches
	// is a page that loads and does nothing, which is why this is a test.
	for _, name := range assetNames() {
		_, err := webFS.ReadFile("web/" + name)
		require.NoErrorf(t, err, "%s is named but not embedded", name)
	}
	referenced, err := referencedAssets()
	require.NoError(t, err)
	for _, name := range referenced {
		assert.Containsf(t, assetNames(), name, "index.html asks for %s, which nothing ships", name)
	}
	assert.Contains(t, referenced, "app.js")
	assert.Contains(t, referenced, "style.css")
}
