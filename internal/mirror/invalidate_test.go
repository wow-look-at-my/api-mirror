package mirror

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// widgetGone is a delivery that says a widget changed in a way the payload
// does not state.
func widgetGone(s *Spec) {
	s.Events = &Events{
		Path:            "/webhook",
		Secret:          testSecret,
		SignatureHeader: "X-Hub-Signature-256",
		TypeHeader:      "X-Event",
		List: []*Event{{
			Type:       "widget_changed",
			Resource:   "widget",
			Subject:    "widget:{{ .payload.id }}",
			Unordered:  true,
			Keys:       []Set{{Field: "id", From: "id"}},
			Invalidate: &Invalidate{Reason: "the payload names the widget and not its new state"},
		}},
	}
}

func ingestFor(t *testing.T, e *Engine) *Ingest {
	t.Helper()
	in, err := NewIngest(e.spec, e.store, e.vars)
	require.NoError(t, err)
	return in
}

func TestTheReaderAfterAnInvalidationRefetches(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), widgetGone)

	require.Equal(t, http.StatusOK, get(t, e, "/widgets/7").Code)
	deliver(t, ingestFor(t, e), "widget_changed", map[string]any{"id": "7"})

	rec := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, rec.Code, "a dropped row is refetched, never answered as absent")
	assert.Contains(t, rec.Body.String(), "hello")
	assert.EqualValues(t, 2, calls.Load())
}

func TestAnInvalidationClearsAStoredRefusal(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.Write([]byte(`{"id":"7","title":"created"}`))
	}), widgetGone)

	assert.Equal(t, http.StatusNotFound, get(t, e, "/widgets/7").Code)
	deliver(t, ingestFor(t, e), "widget_changed", map[string]any{"id": "7"})

	rec := get(t, e, "/widgets/7")
	require.Equal(t, http.StatusOK, rec.Code, "the delivery says the thing changed, so its not-found verdict goes")
	assert.Contains(t, rec.Body.String(), "created")
}

func TestAStoredRefusalKeepsItsDocument(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Repository access blocked","block":{"reason":"tos","html_url":"https://example.invalid/b"}}`))
	}))

	first := get(t, e, "/widgets/7")
	second := get(t, e, "/widgets/7")
	for _, rec := range []*httptest.ResponseRecorder{first, second} {
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Contains(t, rec.Body.String(), "Repository access blocked")
		assert.Contains(t, rec.Body.String(), `"reason":"tos"`)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	}
	assert.NotContains(t, second.Body.String(), "html_url", "the resource's drop patterns apply to the refusal document too")
}

func TestFreshnessPrefix(t *testing.T) {
	res := &Resource{Name: "r", Keys: []Key{{Name: "owner"}, {Name: "repo"}, {Name: "page"}}}

	prefix, exact, ok := freshnessPrefix(res, map[string]string{"owner": "acme", "repo": "a/b"}, false)
	require.True(t, ok)
	assert.Equal(t, "acme/a%2Fb/", prefix, "every component ends at a separator")
	assert.Empty(t, exact)

	prefix, exact, ok = freshnessPrefix(res, map[string]string{"owner": "acme", "repo": "x", "page": "2"}, false)
	require.True(t, ok)
	assert.Equal(t, "acme/x/2", exact)
	assert.Equal(t, "acme/x/2/", prefix)

	_, _, ok = freshnessPrefix(res, map[string]string{"repo": "x"}, false)
	assert.False(t, ok, "a key with a gap before it is no text prefix")

	prefix, _, ok = freshnessPrefix(res, nil, true)
	require.True(t, ok)
	assert.Empty(t, prefix)
}
