package mirror

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func subscribedEngine(t *testing.T, answer func(w http.ResponseWriter)) (*Engine, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		answer(w)
	}), func(s *Spec) {
		s.Events = &Events{
			Path: "/hook", Secret: "shh", SignatureHeader: "X-Sig", TypeHeader: "X-Event",
			List: []*Event{
				{Type: "a.changed", Resource: "widget", Subject: "x", Unordered: true,
					Keys: []Set{{Field: "id", From: "id"}}, Invalidate: &Invalidate{Reason: "r"}},
				{Type: "b.changed", Resource: "widget", Subject: "y", Unordered: true,
					Keys: []Set{{Field: "id", From: "id"}}, Invalidate: &Invalidate{Reason: "r"}},
			},
			Subscribed: &Subscribed{Method: "GET", Path: "/app", Field: "events"},
		}
	})
	return e, &calls
}

func TestSubscribedNamesTheDeclaredTypesTheProviderDoesNotSend(t *testing.T) {
	e, calls := subscribedEngine(t, func(w http.ResponseWriter) { w.Write([]byte(`{"events":["a.changed"]}`)) })
	view := e.subscribedView(context.Background())
	require.True(t, view.Checked)
	assert.Equal(t, []string{"b.changed"}, view.Missing)

	e.subscribedView(context.Background())
	assert.EqualValues(t, 1, calls.Load(), "the answer is reused for a while")
}

func TestSubscribedUnknownNamesNothingMissing(t *testing.T) {
	e, _ := subscribedEngine(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) })
	view := e.subscribedView(context.Background())
	assert.False(t, view.Checked)
	assert.Empty(t, view.Missing, "an unknown list must never read as every type missing")
	assert.Contains(t, view.Error, "401")
}
