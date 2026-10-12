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

// runSpec adds a document resource that a delivery writes whole, and a flag
// that empties one array of its answer.
func runSpec(s *Spec) {
	s.Resources = append(s.Resources, &Resource{
		Name:   "run",
		Store:  StoreDocument,
		TTL:    time.Hour,
		Keys:   []Key{{Name: "id"}},
		Reveal: &Reveal{Public: "true"},
	})
	s.Routes = append(s.Routes, &Route{
		Method: "GET", Path: "/runs/{id}", Resource: "run",
		Query: []QueryParam{{Name: "exclude", Type: FieldBool, Default: "false", Empties: "pulls"}},
	})
	s.Events = &Events{
		Path: "/webhook", Secret: testSecret, SignatureHeader: "X-Hub-Signature-256", TypeHeader: "X-Event",
		List: []*Event{{
			Type: "run", Resource: "run", Subject: "run:{{ .payload.run.id }}", Clock: "run.updated_at",
			Keys:     []Set{{Field: "id", From: "run.id"}},
			Document: "run",
		}},
	}
}

func decodeDoc(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	return doc
}

func TestADeliveryWritesTheWholeDocument(t *testing.T) {
	var calls atomic.Int32
	var sawQuery atomic.Value
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sawQuery.Store(r.URL.RawQuery)
		w.Write([]byte(`{"id":5,"status":"queued","pulls":[{"number":1}],"updated_at":"2026-01-01T00:00:00Z"}`))
	}), runSpec)

	cold := get(t, e, "/runs/5?exclude=true")
	require.Equal(t, http.StatusOK, cold.Code, cold.Body.String())
	assert.Equal(t, "", sawQuery.Load(), "the flag never goes upstream: the stored answer is the full one")
	assert.Equal(t, []any{}, decodeDoc(t, cold.Body.Bytes())["pulls"])

	deliver(t, ingestFor(t, e), "run", map[string]any{"run": map[string]any{
		"id": 5, "status": "completed", "pulls": []any{map[string]any{"number": 1}}, "updated_at": "2026-01-01T00:05:00Z",
	}})

	hit := get(t, e, "/runs/5")
	require.Equal(t, http.StatusOK, hit.Code)
	doc := decodeDoc(t, hit.Body.Bytes())
	assert.Equal(t, "completed", doc["status"], "the delivery is the new answer")
	assert.Len(t, doc["pulls"], 1, "an unflagged read gets the full answer")
	assert.EqualValues(t, 1, calls.Load(), "nothing was fetched to learn what the delivery stated")

	flagged := get(t, e, "/runs/5?exclude=true")
	assert.Equal(t, []any{}, decodeDoc(t, flagged.Body.Bytes())["pulls"])
	assert.Equal(t, "completed", decodeDoc(t, flagged.Body.Bytes())["status"])
}

func TestADeliveryWithNoDocumentFails(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), runSpec, func(s *Spec) {
		ev := s.Events.List[0]
		ev.Clock, ev.Unordered, ev.Subject = "", true, "run"
		ev.Keys = []Set{{Field: "id", From: "id"}}
	})
	w := deliver(t, ingestFor(t, e), "run", map[string]any{"id": 5, "run": map[string]any{"id": 5}})
	assert.Equal(t, http.StatusOK, w.Code, "an object at the path applies")

	w = deliver(t, ingestFor(t, e), "run", map[string]any{"id": 5, "run": "not an object"})
	assert.Equal(t, http.StatusInternalServerError, w.Code, "a payload with no object there must fail, not leave the old row serving")
}

func TestValidateDocumentEvents(t *testing.T) {
	s := testSpec("http://example.invalid")
	runSpec(s)
	require.NoError(t, s.validate())

	s.Events.List[0].Resource = "widget"
	s.Events.List[0].Keys = []Set{{Field: "id", From: "run.id"}}
	assert.ErrorContains(t, s.validate(), "stores columns")

	s = testSpec("http://example.invalid")
	runSpec(s)
	s.Events.List[0].Invalidate = &Invalidate{Reason: "r"}
	assert.ErrorContains(t, s.validate(), "already replaces the row")
}

func TestValidateEmpties(t *testing.T) {
	s := testSpec("http://example.invalid")
	runSpec(s)
	s.Routes[2].Query[0].Type = FieldText
	assert.ErrorContains(t, s.validate(), `needs type="bool"`)

	s = testSpec("http://example.invalid")
	runSpec(s)
	s.Routes[2].Query[0].Key = true
	assert.ErrorContains(t, s.validate(), "cannot key it")
}
