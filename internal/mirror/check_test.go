package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collect runs a check and returns every verdict it emitted.
func collect(t *testing.T, e *Engine, kind string, repair bool) []KeyCheck {
	t.Helper()
	var out []KeyCheck
	require.NoError(t, e.Check(context.Background(), kind, repair, func(k KeyCheck) { out = append(out, k) }))
	return out
}

// seedWidget stores row from the upstream, the way a consumer's read does.
func seedWidget(t *testing.T, e *Engine) {
	t.Helper()
	require.Equal(t, http.StatusOK, get(t, e, "/widgets/7").Code)
}

func TestCheckAgreesWhenTheUpstreamStillSaysTheSameThing(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))
	seedWidget(t, e)

	got := collect(t, e, "widget", false)
	require.Len(t, got, 1)
	assert.Equal(t, CheckAgrees, got[0].Verdict)
	assert.Empty(t, got[0].Diffs)
}

// The whole reason this exists: a value the mirror never learned was wrong.
// Nothing in the traffic, the delivery log or the freshness state shows it.
func TestCheckFindsAFactTheMirrorNeverLearnedWasWrong(t *testing.T) {
	title := atomic.Pointer[string]{}
	first := "hi"
	title.Store(&first)
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Marshalled, never spliced.
		w.Write(deliveryBody(t, map[string]any{"id": "7", "title": *title.Load()}))
	}))
	seedWidget(t, e)

	// The upstream moves on and no delivery arrives, which is exactly the gap.
	moved := "renamed upstream"
	title.Store(&moved)

	got := collect(t, e, "widget", false)
	require.Len(t, got, 1)
	require.Equal(t, CheckDrifted, got[0].Verdict, got[0].Detail)
	require.Len(t, got[0].Diffs, 1)
	assert.Equal(t, "title", got[0].Diffs[0].Field)
	assert.Equal(t, "hi", got[0].Diffs[0].Stored)
	assert.Equal(t, "renamed upstream", got[0].Diffs[0].Upstream)
	assert.False(t, got[0].Repaired, "a plain check reports; it does not write")

	// Reading still serves the stale row, because a check is not a fetch.
	assert.Contains(t, get(t, e, "/widgets/7").Body.String(), "hi")

	repaired := collect(t, e, "widget", true)
	require.Len(t, repaired, 1)
	assert.True(t, repaired[0].Repaired)
	assert.Contains(t, get(t, e, "/widgets/7").Body.String(), "renamed upstream")
}

// A 5xx says nothing about whether the stored row is right, so it must never
// be counted as drift -- and must never be repaired over.
func TestCheckNeverCallsATransientFailureDrift(t *testing.T) {
	var down atomic.Bool
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "later", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))
	seedWidget(t, e)
	down.Store(true)

	got := collect(t, e, "widget", true)
	require.Len(t, got, 1)
	assert.Equal(t, CheckUnreachable, got[0].Verdict)
	assert.False(t, got[0].Repaired)
	assert.Contains(t, get(t, e, "/widgets/7").Body.String(), "hi",
		"an unreachable upstream must not cost the row that was already right")
}

func TestCheckReportsWhatTheUpstreamNoLongerHas(t *testing.T) {
	var gone atomic.Bool
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if gone.Load() {
			http.NotFound(w, nil)
			return
		}
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))
	seedWidget(t, e)
	gone.Store(true)

	got := collect(t, e, "widget", false)
	require.Len(t, got, 1)
	assert.Equal(t, CheckGone, got[0].Verdict)
}

// A key that cannot be checked is named, never skipped: quietly left out
// reads as that agreed.
func TestCheckNamesAKeyItCannotCheck(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"id":"1","title":"a"}]`))
	}), func(s *Spec) {
		s.Routes = append(s.Routes, &Route{
			Method: http.MethodGet, Path: "/widgets", Resource: "widget", List: true,
		})
	})
	require.Equal(t, http.StatusOK, get(t, e, "/widgets").Code)

	got := collect(t, e, "widget:list", false)
	require.Len(t, got, 1)
	assert.Equal(t, CheckUnsupported, got[0].Verdict)
	assert.Contains(t, got[0].Detail, "set")
}

func TestCheckStreamsEachKeyAndThenTheSummary(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))
	seedWidget(t, e)

	rec := adminGet(t, e, defaultDashboardPath+"/api/check?kind=widget&stream=1")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "ndjson")

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	require.Len(t, lines, 2, "one line per key, then the summary")

	var first KeyCheck
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, CheckAgrees, first.Verdict)

	var summary CheckSummary
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &summary))
	assert.Equal(t, 1, summary.Checked)
	assert.Equal(t, 1, summary.Verdicts[CheckAgrees])
	assert.NotEmpty(t, summary.Elapsed)
}

func TestCheckRepairsOnlyOnAPost(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))

	rec := adminGet(t, e, defaultDashboardPath+"/api/check")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a check with no kind has nothing to ask about")

	// apply=true on a GET is still a read.
	seedWidget(t, e)
	var summary CheckSummary
	body := adminGet(t, e, defaultDashboardPath+"/api/check?kind=widget&apply=true")
	require.Equal(t, http.StatusOK, body.Code)
	require.NoError(t, json.Unmarshal(body.Body.Bytes(), &summary))
	assert.Zero(t, summary.Repaired)
}

func TestCheckIsEmptyForAKindNothingHolds(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, defaultDashboardPath+"/api/check?kind=widget", nil)
	r.Header.Set("X-Mirror-Token", e.admin.token)
	e.ServeHTTP(rec, r)

	require.Equal(t, http.StatusOK, rec.Code)
	var summary CheckSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Zero(t, summary.Checked)
	assert.Empty(t, summary.Keys)
}
