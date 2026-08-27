package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The background half: the sweep that keeps keys warm, the replayer that asks
// for lost deliveries back, the fan-out that tells subscribers, and the window
// that stops ten identical unmodelled questions costing ten answers.

func TestRefreshSweepsAKeyNobodyIsReading(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), func(s *Spec) {
		s.Refresh = &Refresh{Interval: time.Hour}
	})
	require.True(t, e.refresh.Enabled())
	assert.Contains(t, e.refresh.kinds, "widget", "a spec that names no kinds sweeps every routed resource")

	require.Equal(t, http.StatusOK, get(t, e, "/widgets/7").Code)
	require.Equal(t, int32(1), calls.Load())

	// The sweep exists for the key nobody is asking for.
	expire(t, e, "widget", "7")
	e.refresh.cycle()

	assert.Equal(t, int32(2), calls.Load(), "the sweep refetched a key with no consumer waiting on it")
	assert.Equal(t, 1, e.refresh.Stats().Swept)
	assert.Zero(t, e.refresh.Stats().Errors)
}

func TestRefreshLeavesACredentialKeyedRowAlone(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Resources = append(s.Resources, &Resource{
			Name:   "mine",
			Store:  StoreColumns,
			TTL:    time.Hour,
			Keys:   []Key{{Name: "caller", Credential: true}, {Name: "id", From: "id"}},
			Fields: []Field{{Name: "title", Type: FieldText, From: "title"}},
			Reveal: &Reveal{Credential: true},
		})
		s.Routes = append(s.Routes, &Route{Method: "GET", Path: "/mine/{id}", Resource: "mine"})
		s.Refresh = &Refresh{Interval: time.Hour}
	})

	_, ok := e.planFor("mine", StaleKey{Key: "abc/7"})
	assert.False(t, ok,
		"the sweep has no credential, so refreshing this would file the mirror's own answer under a caller's key")
}

func TestRefreshStatsSayWhenTheSweepIsFailing(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}), func(s *Spec) { s.Refresh = &Refresh{Interval: time.Hour} })

	get(t, e, "/widgets/7")
	expire(t, e, "widget", "7")
	e.refresh.cycle()

	stats := e.refresh.Stats()
	assert.Positive(t, stats.Errors,
		"a sweep failing every cycle for a day looks exactly like a healthy one from outside")
	assert.True(t, stats.Enabled)
}

// expire ages one key's freshness row out, so the sweep has something to do.
func expire(t *testing.T, e *Engine, kind, key string) {
	t.Helper()
	_, err := e.store.db.Exec(
		`UPDATE mirror_freshness SET expires_at = ? WHERE kind = ? AND key = ?`,
		time.Now().Add(-time.Hour).Unix(), kind, key)
	require.NoError(t, err)
}

func TestReplayAsksForEachLostDeliveryOnce(t *testing.T) {
	var asked []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/hook/deliveries":
			w.Write([]byte(`[{"id":101,"delivered_at":"2999-01-01T00:00:00Z"},` +
				`{"id":102,"delivered_at":"2999-01-01T00:00:00Z"}]`))
		default:
			asked = append(asked, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(upstream.Close)

	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Upstream.Base = upstream.URL
		s.Replay = &Replay{
			Interval:  5 * time.Minute,
			List:      "/app/hook/deliveries",
			Redeliver: "/app/hook/deliveries/{{ .id }}/attempts",
			Method:    http.MethodPost,
			ID:        "id",
			At:        "delivered_at",
		}
	})
	require.True(t, e.replay.Enabled())

	e.replay.cycle()
	require.Len(t, asked, 2)
	assert.Contains(t, asked, "/app/hook/deliveries/101/attempts")

	// A second cycle asks for nothing: twice makes recovery a duplicate source.
	e.replay.cycle()
	assert.Len(t, asked, 2, "once per delivery, not once per cycle")

	stats := e.replay.Stats()
	assert.Equal(t, 2, stats.Resent)
	assert.Equal(t, 4, stats.Found)
	assert.Zero(t, stats.Errors)
}

func TestReplaySkipsWhatIsPastTheLookback(t *testing.T) {
	var asked int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/deliveries" {
			w.Write([]byte(`[{"id":1,"at":"2000-01-01T00:00:00Z"}]`))
			return
		}
		asked++
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstream.Close)

	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Upstream.Base = upstream.URL
		s.Replay = &Replay{
			Interval: time.Minute, List: "/deliveries",
			Redeliver: "/deliveries/{{ .id }}", ID: "id", At: "at", Lookback: time.Hour,
		}
	})
	e.replay.cycle()
	assert.Zero(t, asked, "the provider will not re-send something older than it keeps either")
}

func TestReplayIsOffWhenTheSpecDeclaresNone(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	assert.False(t, e.replay.Enabled())
	assert.False(t, e.replay.Stats().Enabled,
		"a mirror with no replay is valid; one whose author did not realise that was a choice is not")
}

func TestReplayDoesNotRunWithoutTheCredentialItRequires(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)

	declare := func(s *Spec) {
		s.Upstream.Base = upstream.URL
		s.Replay = &Replay{
			Interval: time.Minute, List: "/deliveries", Redeliver: "/deliveries/{{ .id }}",
			ID: "id", Requires: "env.MIRROR_TEST_REPLAY_TOKEN",
		}
	}

	e, _ := newTestEngine(t, http.NotFoundHandler(), declare)
	assert.False(t, e.replay.Enabled())
	assert.Equal(t, "env.MIRROR_TEST_REPLAY_TOKEN", e.replay.Stats().Off,
		"a declared job that is not running has to say which value would start it")

	e.replay.Start()
	t.Cleanup(e.replay.Stop)
	assert.Zero(t, calls, "a cycle that can only fail is not a cycle worth running every interval forever")

	t.Setenv("MIRROR_TEST_REPLAY_TOKEN", "sekrit")
	with, _ := newTestEngine(t, http.NotFoundHandler(), declare)
	assert.True(t, with.replay.Enabled())
	assert.Empty(t, with.replay.Stats().Off)
}

func TestNotifierSignsAndTellsSubscribersAfterTheWriteLands(t *testing.T) {
	var got struct {
		body      []byte
		signature string
	}
	done := make(chan struct{})
	subscriber := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _, _ = readCapped(r.Body)
		got.signature = r.Header.Get("X-Hub-Signature-256")
		close(done)
	}))
	t.Cleanup(subscriber.Close)

	e := notifyEngine(t)
	sub, err := e.notify.Subs().Create(context.Background(), "token:abc", subscriber.URL, nil)
	require.NoError(t, err)

	e.notify.Fan(context.Background(), &Delivery{
		ID: "d1", Type: "widget.changed", Subject: "widget:7",
		Event: &Event{Type: "widget.changed", Resource: "widget"},
	}, DeliveryApplied)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber was never told")
	}
	require.True(t, e.notify.Drain(5*time.Second))

	var note Notification
	require.NoError(t, json.Unmarshal(got.body, &note))
	assert.Equal(t, "widget.changed", note.Type)
	assert.Equal(t, "widget:7", note.Subject)
	assert.Equal(t, DeliveryApplied, note.Disposition)
	assert.True(t, verifyDelivery([]byte(sub.Secret), got.signature, got.body),
		"the signature is the only thing telling the subscriber this came from us")
}

func TestNotifierParksASubscriptionThatKeepsFailing(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	e := notifyEngine(t)
	e.notify.rule.Retries = 0
	e.notify.rule.DisableAfter = 1
	sub, err := e.notify.Subs().Create(context.Background(), "token:abc", dead.URL, nil)
	require.NoError(t, err)

	e.notify.Fan(context.Background(), &Delivery{
		ID: "d1", Type: "widget.changed", Event: &Event{Type: "widget.changed", Resource: "widget"},
	}, DeliveryApplied)
	require.True(t, e.notify.Drain(10*time.Second))

	all, err := e.notify.Subs().All(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, sub.ID, all[0].ID)
	assert.True(t, all[0].Disabled)
	assert.NotEmpty(t, all[0].LastError,
		"parking is loud: an operator must see a consumer that stopped being told")
}

func TestSubscriptionEventFilterDefaultsToEverything(t *testing.T) {
	e := notifyEngine(t)
	subs := e.notify.Subs()
	_, err := subs.Create(context.Background(), "token:a", "https://a.example/hook", nil)
	require.NoError(t, err)
	_, err = subs.Create(context.Background(), "token:b", "https://b.example/hook", []string{"other.event"})
	require.NoError(t, err)

	matched, err := subs.Matching(context.Background(), "widget.changed")
	require.NoError(t, err)
	require.Len(t, matched, 1,
		"a subscriber who named nothing asked for everything; one who named a type asked for that type")
	assert.Equal(t, "token:a", matched[0].Principal)
	assert.NotEmpty(t, matched[0].Secret, "the fan-out needs the secret to sign with")
}

func TestSubscriptionDeleteIsScopedToItsOwner(t *testing.T) {
	e := notifyEngine(t)
	subs := e.notify.Subs()
	mine, err := subs.Create(context.Background(), "token:mine", "https://a.example/hook", nil)
	require.NoError(t, err)

	gone, err := subs.Delete(context.Background(), "token:someone-else", mine.ID)
	require.NoError(t, err)
	assert.False(t, gone, "naming somebody's id must not be enough to delete their registration")

	gone, err = subs.Delete(context.Background(), "token:mine", mine.ID)
	require.NoError(t, err)
	assert.True(t, gone)
}

func TestSubscriptionAPIRefusesACallbackThatIsNotAnEndpoint(t *testing.T) {
	for _, raw := range []string{"", "file:///etc/passwd", "gopher://x", "not a url at all", "https://"} {
		assert.Errorf(t, checkCallbackURL(raw),
			"%q names where this server will send a signed request", raw)
	}
	assert.NoError(t, checkCallbackURL("https://consumer.example/hook"))
}

func TestSubscriptionAPICreatesAndReturnsTheSecretOnce(t *testing.T) {
	e := notifyEngine(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, defaultDashboardPath+"/api/subscriptions",
		strings.NewReader(`{"url":"https://consumer.example/hook","events":["widget.changed"]}`))
	r.Header.Set("X-Mirror-Token", e.admin.token)
	e.ServeHTTP(rec, r)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created subscriptionCreated
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.NotEmpty(t, created.Secret, "this is the only moment the subscriber can still be given it")

	listed := adminJSON[[]Subscription](t, e, defaultDashboardPath+"/api/subscriptions")
	require.Len(t, listed, 1)
	assert.Empty(t, listed[0].Secret, "the store never reads a secret back out to a listing")
}

// notifyEngine is an engine with subscriber notifications declared, backed by a
// config database in a temp directory.
func notifyEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Events = &Events{
			Path: "/hook", Secret: "shh",
			SignatureHeader: "X-Hub-Signature-256", TypeHeader: "X-Event",
			List: []*Event{{
				Type: "widget.changed", Resource: "widget",
				Subject: "widget:{{ .payload.id }}", Clock: "updated_at",
				Sets: []Set{{Field: "title", From: "title"}},
			}},
		}
		s.Notify = &Notify{Path: "/_mirror/subs", DB: filepath.Join(dir, "subs.db"), Retries: 0}
	})
	t.Cleanup(func() { e.notify.Close() })
	return e
}

func TestDebounceSharesOneAnswerAcrossIdenticalReads(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Write([]byte(`{"shared":true}`))
	}))
	t.Cleanup(upstream.Close)

	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Upstream.Base = upstream.URL
		s.Upstream.Debounce = 2 * time.Second
	})
	require.NotNil(t, e.debounce)

	const waiters = 4
	done := make(chan *httptest.ResponseRecorder, waiters)
	for range waiters {
		go func() { done <- get(t, e, "/unmodelled/thing") }()
	}
	// Let every waiter reach the debouncer before the leader's call returns.
	time.Sleep(150 * time.Millisecond)
	close(release)

	for range waiters {
		rec := <-done
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "shared")
	}
	assert.Equal(t, int32(1), calls.Load(),
		"four consumers asking one unmodelled question must cost the budget one answer")
}

func TestDebounceNeverSharesAcrossCredentials(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/thing", nil)
	assert.True(t, shareable(r))

	r.Header.Set("Authorization", "Bearer somebody")
	assert.False(t, shareable(r),
		"handing one caller's answer to another is the one failure this whole engine exists to prevent")

	post := httptest.NewRequest(http.MethodPost, "/thing", nil)
	assert.False(t, shareable(post))
}

func TestZeroDebounceForwardsImmediately(t *testing.T) {
	assert.Nil(t, NewDebouncer(0), "forwarding at once is a real choice, and a nil says so by having nothing to hold with")
}

func TestSubscriptionDBTravelsWithTheCache(t *testing.T) {
	assert.Equal(t, "/var/lib/mirror/github-subscriptions.db",
		subscriptionsPath("", "/var/lib/mirror/github.db"),
		"one -db flag must not put the cache in one place and its subscriptions in the working directory")
	assert.Equal(t, "/etc/mirror/subs.db", subscriptionsPath("/etc/mirror/subs.db", "/var/lib/mirror/github.db"),
		"a declared path is the operator's decision")
}
