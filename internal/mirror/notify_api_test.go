package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subsEngine is notifyEngine with a public subscription path and a
// scope field, the way the GitHub spec declares them.
func subsEngine(t *testing.T) *Engine {
	t.Helper()
	e := notifyEngine(t)
	e.spec.Notify.ScopeField = "repos"
	return e
}

func callSubs(t *testing.T, e *Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestSubscriptionAPIIsScopedToTheCaller(t *testing.T) {
	e := subsEngine(t)

	anon := callSubs(t, e, http.MethodGet, "/_mirror/subs", "", nil)
	assert.Equal(t, http.StatusUnauthorized, anon.Code, "a registration must belong to somebody")

	created := callSubs(t, e, http.MethodPost, "/_mirror/subs", "token alice", map[string]any{
		"url": "https://alice.example/hook", "repos": []string{"acme/widget"}, "secret": "0123456789abcdef-alice",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var sub map[string]any
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &sub))
	id, _ := sub["id"].(string)
	require.NotEmpty(t, id)
	assert.Equal(t, "0123456789abcdef-alice", sub["secret"], "a subscriber's own secret is kept")
	assert.Equal(t, []any{"acme/widget"}, sub["repos"], "the scope filter travels under the spec's field name")
	assert.Equal(t, true, sub["active"])

	mine := callSubs(t, e, http.MethodGet, "/_mirror/subs/"+id, "token alice", nil)
	assert.Equal(t, http.StatusOK, mine.Code)
	assert.NotContains(t, mine.Body.String(), "0123456789abcdef-alice", "the secret is returned once, at creation")

	theirs := callSubs(t, e, http.MethodGet, "/_mirror/subs/"+id, "token bob", nil)
	assert.Equal(t, http.StatusNotFound, theirs.Code, "another caller's registration reads as absent")

	list := callSubs(t, e, http.MethodGet, "/_mirror/subs", "token bob", nil)
	assert.JSONEq(t, `[]`, list.Body.String())

	patched := callSubs(t, e, http.MethodPatch, "/_mirror/subs/"+id, "token alice", map[string]any{"active": false, "repos": []string{}})
	require.Equal(t, http.StatusOK, patched.Code, patched.Body.String())
	require.NoError(t, json.Unmarshal(patched.Body.Bytes(), &sub))
	assert.Equal(t, false, sub["active"])
	assert.Equal(t, []any{}, sub["repos"])

	short := callSubs(t, e, http.MethodPatch, "/_mirror/subs/"+id, "token alice", map[string]any{"secret": "short"})
	assert.Equal(t, http.StatusBadRequest, short.Code)

	gone := callSubs(t, e, http.MethodDelete, "/_mirror/subs/"+id, "token bob", nil)
	assert.Equal(t, http.StatusNotFound, gone.Code)
	gone = callSubs(t, e, http.MethodDelete, "/_mirror/subs/"+id, "token alice", nil)
	assert.Equal(t, http.StatusNoContent, gone.Code)
}

// collector is a subscriber endpoint that records every notification.
type collector struct {
	mu    sync.Mutex
	notes []map[string]any
}

func (c *collector) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	body, _, _ := readCapped(r.Body)
	var note map[string]any
	_ = json.Unmarshal(body, &note)
	c.mu.Lock()
	c.notes = append(c.notes, note)
	c.mu.Unlock()
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.notes)
}

func TestNotificationsAreFilteredByScopeAndByWhatTheSubscriberCanSee(t *testing.T) {
	e := notifyEngine(t)
	e.notify.rule.Scope = "{{ .payload.repo }}"
	e.notify.rule.Reveal = &NotifyReveal{Resource: "widget", Keys: []Set{{Field: "id", From: "id"}}}

	granted, denied, scoped := &collector{}, &collector{}, &collector{}
	grantedSrv := httptest.NewServer(granted)
	deniedSrv := httptest.NewServer(denied)
	scopedSrv := httptest.NewServer(scoped)
	t.Cleanup(grantedSrv.Close)
	t.Cleanup(deniedSrv.Close)
	t.Cleanup(scopedSrv.Close)

	ctx := context.Background()
	subs := e.notify.Subs()
	_, err := subs.Create(ctx, "token:granted", grantedSrv.URL, "", nil, nil)
	require.NoError(t, err)
	_, err = subs.Create(ctx, "token:denied", deniedSrv.URL, "", nil, nil)
	require.NoError(t, err)
	_, err = subs.Create(ctx, "token:granted", scopedSrv.URL, "", nil, []string{"other/repo"})
	require.NoError(t, err)

	// widget is public in testSpec, so make it private for this test.
	widget, ok := e.store.Resource("widget")
	require.True(t, ok)
	widget.Reveal = &Reveal{GrantTTL: time.Hour}
	require.NoError(t, e.store.RecordGrant(ctx, Grant{
		Principal: "token:granted", Resource: "widget", Key: keyString(widget, map[string]string{"id": "7"}),
		Source: "probe", ExpiresAt: time.Now().Add(time.Hour),
	}))

	e.notify.Fan(ctx, &Delivery{
		ID: "d1", Type: "widget.changed",
		Payload: map[string]any{"id": "7", "repo": "ACME/Widget"},
		Event:   &Event{Type: "widget.changed", Resource: "widget"},
	}, DeliveryApplied)
	require.True(t, e.notify.Drain(5*time.Second))

	assert.Equal(t, 1, granted.count(), "a subscriber with a grant for the row hears about it")
	assert.Zero(t, denied.count(), "a subscriber that cannot read the row must not learn it changed")
	assert.Zero(t, scoped.count(), "a scope filter that does not list the delivery's scope drops it")
}

func TestADeliveryNotifiesOnceAfterEveryHandlerApplies(t *testing.T) {
	spec := fanOutSpec()
	spec.Notify = &Notify{DB: t.TempDir() + "/subs.db"}
	require.NoError(t, spec.validate())
	in, store := newIngest(t, spec)
	n, err := NewNotifier(spec, map[string]any{}, nil, store.path)
	require.NoError(t, err)
	t.Cleanup(func() { n.Close() })
	in.SetNotifier(n)

	c := &collector{}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	_, err = n.Subs().Create(context.Background(), "token:a", srv.URL, "", nil, nil)
	require.NoError(t, err)

	deliver(t, in, "repository", repoDelivery("acme", "widget", clockLate, map[string]any{"visibility": "public"}))
	require.True(t, n.Drain(5*time.Second))
	assert.Equal(t, 1, c.count(), "two handlers of one delivery are one notification")
}
