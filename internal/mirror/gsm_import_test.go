package mirror

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gsmRowFixture is one row of a github-state-mirror registry, in its own column spelling.
type gsmRowFixture struct {
	id, principal, url, secret, repos, events string
	active, failures                          int
	disabledReason, created, updated          string
	lastSuccess, lastFailure, lastError       string
}

func gsmRowOf(id, principal, url string) gsmRowFixture {
	return gsmRowFixture{
		id: id, principal: principal, url: url, secret: "0123456789abcdef-" + id,
		repos: "[]", events: "[]", active: 1,
		created: "2026-09-01T10:00:00.123456789Z", updated: "2026-09-02T11:00:00.5Z",
	}
}

// gsmFixture builds a registry file from github-state-mirror's own DDL.
func gsmFixture(t *testing.T, rows ...gsmRowFixture) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("testdata", "gsm_subscriptions.sql"))
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "github-mirror-subscriptions.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(string(ddl))
	require.NoError(t, err)
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO subscriptions (id, principal, url, secret, repo_filters, event_filters, active,
			consecutive_failures, disabled_reason, created_at, updated_at, last_success_at, last_failure_at, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.principal, r.url, r.secret, r.repos, r.events, r.active, r.failures, r.disabledReason,
			r.created, r.updated, r.lastSuccess, r.lastFailure, r.lastError)
		require.NoError(t, err)
	}
	return path
}

// gsmFingerprintOf is github-state-mirror's ghclient.Fingerprint.
func gsmFingerprintOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// gsmIdentities declares the identities github-state-mirror resolves, the way the GitHub spec does.
func gsmIdentities(s *Spec) {
	s.Identity = &Identity{Header: "X-Mirror-Identity", Path: "/app", Scheme: "Bearer", ID: "id", Prefix: "app"}
	s.Whoami = &Whoami{Path: "/user", ID: "id", Prefix: "user"}
}

// importNotifier opens a notifier over subsDB that imports gsmPath through the template language.
func importNotifier(t *testing.T, spec *Spec, subsDB, gsmPath string) (*Notifier, error) {
	t.Helper()
	spec.Notify = &Notify{DB: subsDB, Import: "{{ .env.GSM_SUBSCRIPTIONS_DB }}"}
	n, err := NewNotifier(spec, map[string]any{"env": map[string]any{"GSM_SUBSCRIPTIONS_DB": gsmPath}}, nil, subsDB)
	if n != nil {
		t.Cleanup(func() { n.Close() })
	}
	return n, err
}

func importSpec() *Spec {
	s := &Spec{Name: "github"}
	gsmIdentities(s)
	return s
}

func TestTokenPrincipalMatchesTheGSMFingerprint(t *testing.T) {
	want := "token:" + gsmFingerprintOf("ghs_abc")
	for _, auth := range []string{"Bearer ghs_abc", "token ghs_abc", "bearer ghs_abc"} {
		r := &http.Request{Header: http.Header{"Authorization": {auth}}}
		assert.Equal(t, want, tokenPrincipal(r), "%q names the same credential", auth)
	}
}

func TestGSMImportCarriesEveryField(t *testing.T) {
	row := gsmRowOf("sub_0001", "user:42", "https://ci.example/hook")
	row.repos = `["acme","acme/widget"]`
	row.events = `["push","pull_request"]`
	row.active = 0
	row.failures = 10
	row.disabledReason = "auto-disabled after 10 consecutive delivery failures"
	row.lastSuccess = "2026-09-03T12:00:00Z"
	row.lastFailure = "2026-09-04T13:00:00.25Z"
	row.lastError = "http 502"
	gsm := gsmFixture(t, row)

	n, err := importNotifier(t, importSpec(), filepath.Join(t.TempDir(), "subs.db"), gsm)
	require.NoError(t, err)
	require.NotNil(t, n.imported)
	assert.Equal(t, ImportReport{Source: "github-state-mirror", File: gsm, Imported: 1}, *n.imported)

	all, err := n.Subs().All(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 1)
	got := all[0]
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return v.Truncate(time.Second).UTC()
	}
	assert.Equal(t, Subscription{
		ID: "sub_0001", Principal: "user:42", URL: "https://ci.example/hook",
		Events: []string{"push", "pull_request"}, Scopes: []string{"acme", "acme/widget"},
		CreatedAt: at(row.created), UpdatedAt: at(row.updated),
		LastOK: at(row.lastSuccess), LastFailure: at(row.lastFailure),
		Failures: 10, Disabled: true, LastError: "http 502",
		DisabledReason: "auto-disabled after 10 consecutive delivery failures",
	}, got, "every column survives, timestamps to the second this store keeps")
	secret, err := n.Subs().secretOf(context.Background(), got.ID)
	require.NoError(t, err)
	assert.Equal(t, row.secret, secret, "the subscriber's endpoint already verifies with this secret")
}

func TestGSMImportedSubscriberIsNotifiedUnderItsPrincipal(t *testing.T) {
	const userToken = "gho_user"
	const installToken = "ghs_install"
	user, install, stranger := &signedCollector{}, &signedCollector{}, &signedCollector{}
	serve := func(c *signedCollector) *httptest.Server {
		srv := httptest.NewServer(c)
		t.Cleanup(srv.Close)
		return srv
	}
	userSrv, installSrv, strangerSrv := serve(user), serve(install), serve(stranger)

	userRow := gsmRowOf("sub_user", "user:42", userSrv.URL)
	userRow.repos = `["acme"]`
	installRow := gsmRowOf("sub_install", gsmFingerprintOf(installToken), installSrv.URL)
	strangerRow := gsmRowOf("sub_stranger", "user:99", strangerSrv.URL)
	t.Setenv("GSM_SUBSCRIPTIONS_DB", gsmFixture(t, userRow, installRow, strangerRow))

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer "+userToken {
			w.Write([]byte(`{"id":42}`))
			return
		}
		http.NotFound(w, r)
	})
	dir := t.TempDir()
	e, _ := newTestEngine(t, upstream, func(s *Spec) {
		gsmIdentities(s)
		s.Events = &Events{
			Path: "/hook", Secret: "shh", SignatureHeader: "X-Hub-Signature-256", TypeHeader: "X-Event",
			List: []*Event{{
				Type: "widget.changed", Resource: "widget",
				Subject: "widget:{{ .payload.id }}", Clock: "updated_at",
				Keys: []Set{{Field: "id", From: "id"}},
				Sets: []Set{{Field: "title", From: "title"}},
			}},
		}
		s.Notify = &Notify{
			Path: "/_mirror/subs", DB: filepath.Join(dir, "subs.db"), ScopeField: "repos",
			Import: "{{ .env.GSM_SUBSCRIPTIONS_DB }}",
			Scope:  "{{ .payload.repo }}",
			Reveal: &NotifyReveal{Resource: "widget", Keys: []Set{{Field: "id", From: "id"}}},
		}
	})
	t.Cleanup(func() { e.notify.Close() })

	ctx := context.Background()
	widget, ok := e.store.Resource("widget")
	require.True(t, ok)
	widget.Reveal = &Reveal{GrantTTL: time.Hour}
	for _, p := range []string{"user:42", "token:" + gsmFingerprintOf(installToken)} {
		require.NoError(t, e.store.RecordGrant(ctx, Grant{
			Principal: p, Resource: "widget", Key: keyString(widget, map[string]string{"id": "7"}),
			Source: "probe", ExpiresAt: time.Now().Add(time.Hour),
		}))
	}

	mine := callSubs(t, e, http.MethodGet, "/_mirror/subs", "Bearer "+userToken, nil)
	require.Equal(t, http.StatusOK, mine.Code, mine.Body.String())
	var listed []map[string]any
	require.NoError(t, json.Unmarshal(mine.Body.Bytes(), &listed))
	require.Len(t, listed, 1, "the owner manages its imported registration under user:42")
	assert.Equal(t, "sub_user", listed[0]["id"])
	assert.Equal(t, []any{"acme"}, listed[0]["repos"])

	theirs := callSubs(t, e, http.MethodGet, "/_mirror/subs/sub_install", "token "+installToken, nil)
	assert.Equal(t, http.StatusOK, theirs.Code, "a fingerprint owner reaches its row with the same token")

	e.notify.Fan(ctx, &Delivery{
		ID: "d1", Type: "widget.changed",
		Payload: map[string]any{"id": "7", "repo": "ACME/Widget"},
		Event:   &Event{Type: "widget.changed", Resource: "widget"},
	}, DeliveryApplied)
	require.True(t, e.notify.Drain(5*time.Second))

	require.Len(t, user.bodies, 1, "an owner filter admits every repo of that owner")
	require.Len(t, install.bodies, 1)
	assert.Empty(t, stranger.bodies, "a principal with no proof for the row hears nothing")

	var note map[string]any
	require.NoError(t, json.Unmarshal(user.bodies[0], &note))
	assert.Equal(t, "sub_user", note["subscription_id"], "the subscriber keeps the id it was given")
	assert.True(t, verifyDelivery([]byte(userRow.secret), user.signatures[0], user.bodies[0]),
		"the endpoint verifies with the secret github-state-mirror issued")
	assert.True(t, verifyDelivery([]byte(installRow.secret), install.signatures[0], install.bodies[0]))
}

// signedCollector records each notification body with its signature.
type signedCollector struct {
	mu         sync.Mutex
	bodies     [][]byte
	signatures []string
}

func (c *signedCollector) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	body, _, _ := readCapped(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
	c.signatures = append(c.signatures, r.Header.Get("X-Hub-Signature-256"))
}

// captureHandler keeps every log message.
type captureHandler struct{ msgs *[]string }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func newCaptureLogger(msgs *[]string) *slog.Logger { return slog.New(captureHandler{msgs: msgs}) }

func TestGSMImportRunsOnceAndDoesNotResurrectADelete(t *testing.T) {
	gsm := gsmFixture(t,
		gsmRowOf("sub_a", "user:1", "https://a.example/hook"),
		gsmRowOf("sub_b", "app:7", "https://b.example/hook"))
	subsDB := filepath.Join(t.TempDir(), "subs.db")

	first, err := importNotifier(t, importSpec(), subsDB, gsm)
	require.NoError(t, err)
	assert.Equal(t, 2, first.imported.Imported)
	gone, err := first.Subs().Delete(context.Background(), "app:7", "sub_b")
	require.NoError(t, err)
	require.True(t, gone)
	require.NoError(t, first.Close())

	second, err := importNotifier(t, importSpec(), subsDB, gsm)
	require.NoError(t, err)
	assert.Equal(t, 0, second.imported.Imported, "a re-run imports nothing twice")
	assert.Equal(t, 2, second.imported.Present, "the deleted row is still a row the ledger has seen")
	all, err := second.Subs().All(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 1, "a subscriber deleted here stays deleted")
	assert.Equal(t, "sub_a", all[0].ID)
}

func TestGSMImportFailsAnUntranslatableRowLoudly(t *testing.T) {
	badFilter := gsmRowOf("sub_badfilter", "user:3", "https://c.example/hook")
	badFilter.repos = `not json`
	gsm := gsmFixture(t,
		gsmRowOf("sub_good", "user:1", "https://a.example/hook"),
		gsmRowOf("sub_install", "app-installation:481", "https://b.example/hook"),
		gsmRowOf("sub_nouser", "user:2", "https://d.example/hook"),
		badFilter)
	spec := importSpec()
	spec.Whoami = nil
	spec.Identity = nil

	var logged []string
	saved := logger
	logger = newCaptureLogger(&logged)
	t.Cleanup(func() { logger = saved })

	subsDB := filepath.Join(t.TempDir(), "subs.db")
	n, err := importNotifier(t, spec, subsDB, gsm)
	require.NoError(t, err, "one bad row must not stop every other subscriber being told")
	assert.Equal(t, 0, n.imported.Imported)
	assert.Equal(t, 4, n.imported.Failed)
	require.Len(t, n.imported.Failures, 4, "rows read in id order: badfilter, good, install, nouser")
	assert.Contains(t, n.imported.Failures[1], "sub_good")
	assert.Contains(t, n.imported.Failures[1], "<whoami")
	assert.Contains(t, n.imported.Failures[2], "sub_install")
	assert.Contains(t, logged, "notify: import row sub_install NOT imported: "+
		`principal "app-installation:481": no caller of this mirror resolves to a "app-installation" principal`)
	assert.Contains(t, logged, "notify: import from "+gsm+": 0 imported, 0 already present, 4 failed")
	all, err := n.Subs().All(context.Background())
	require.NoError(t, err)
	assert.Empty(t, all)
	require.NoError(t, n.Close())

	again, err := importNotifier(t, importSpec(), subsDB, gsm)
	require.NoError(t, err)
	assert.Equal(t, 2, again.imported.Imported, "a failed row is tried again once the spec can resolve it")
	assert.Equal(t, 2, again.imported.Failed)
	require.Len(t, again.imported.Failures, 2)
	assert.Contains(t, again.imported.Failures[0], "sub_badfilter")
	assert.Contains(t, again.imported.Failures[0], "repo_filters", "a filter that does not decode is never read as everything")
	assert.Contains(t, again.imported.Failures[1], `"app-installation"`)
}

func TestGSMImportSourceMustExistWhenNamed(t *testing.T) {
	subsDB := filepath.Join(t.TempDir(), "subs.db")
	_, err := importNotifier(t, importSpec(), subsDB, filepath.Join(t.TempDir(), "absent.db"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.db")

	notGSM := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", notGSM)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE unrelated (x INTEGER)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = importNotifier(t, importSpec(), subsDB, notGSM)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a github-state-mirror subscription registry")

	n, err := importNotifier(t, importSpec(), subsDB, "")
	require.NoError(t, err, "an empty value names nothing")
	assert.Nil(t, n.imported)
}
