package engine

import _ "embed"

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSecret signs deliveries. The spec reads it from the environment, as a host's spec does.
const testSecret = "embed-test-secret"

//go:embed testdata/embed-spec.xml
var embedSpec string

// tap collects what the host callbacks receive. The outbound one runs off the request goroutine.
type tap struct {
	mu        sync.Mutex
	exchanges []Exchange
	requests  []Request
}

func (c *tap) exchange(x Exchange) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exchanges = append(c.exchanges, x)
}

func (c *tap) request(r Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r)
}

func (c *tap) fetches() []Exchange {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Exchange
	for _, x := range c.exchanges {
		if x.Lane == "fetch" {
			out = append(out, x)
		}
	}
	return out
}

func (c *tap) lastRequest(t *testing.T) Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.requests)
	return c.requests[len(c.requests)-1]
}

// newEmbedded builds an engine from spec bytes against a counting upstream.
func newEmbedded(t *testing.T) (*Engine, *tap, *atomic.Int32) {
	t.Helper()
	t.Setenv("EMBED_TEST_SECRET", testSecret)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/tags" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"v1"}]`))
	}))
	t.Cleanup(up.Close)

	c := &tap{}
	src := strings.Replace(embedSpec, "UPSTREAM", up.URL, 1)
	e, err := New(context.Background(), "embedded.xml", []byte(src), Options{
		DB:         filepath.Join(t.TempDir(), "embed.db"),
		OnExchange: c.exchange,
		OnRequest:  c.request,
	})
	require.NoError(t, err)
	return e, c, &calls
}

func get(t *testing.T, e *Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestDeclares_AnswersOnlyForSpecRoutes(t *testing.T) {
	e, _, _ := newEmbedded(t)
	t.Cleanup(func() { e.Close() })

	assert.True(t, e.Declares(http.MethodGet, "/repos/acme/widget/tags"), "the declared route")
	assert.False(t, e.Declares(http.MethodPost, "/repos/acme/widget/tags"), "a method no route declares")
	assert.False(t, e.Declares(http.MethodGet, "/repos/acme/widget/branches"), "a path no route declares")
	assert.False(t, e.Declares(http.MethodGet, "/repos/acme/widget"), "a prefix of a declared route")
	assert.False(t, e.Declares(http.MethodGet, "/_mirror/"), "the dashboard is not a data route")
	assert.False(t, e.Declares(http.MethodPost, "/webhook"), "the events path is not a data route")
	assert.Equal(t, "/webhook", e.EventsPath())
}

func TestEmbedded_MissThenHitThenDeliveryInvalidates(t *testing.T) {
	e, c, calls := newEmbedded(t)
	e.Start()

	first := get(t, e, "/repos/acme/widget/tags")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.JSONEq(t, `[{"name":"v1"}]`, first.Body.String())
	assert.Equal(t, DispositionMiss, c.lastRequest(t).Disposition)
	assert.Equal(t, "/repos/{owner}/{repo}/tags", c.lastRequest(t).Shape)

	second := get(t, e, "/repos/acme/widget/tags")
	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String(), "a hit rebuilds the same answer")
	assert.Equal(t, DispositionHit, c.lastRequest(t).Disposition)
	assert.EqualValues(t, 1, calls.Load(), "the hit makes no upstream call")

	require.Eventually(t, func() bool { return len(c.fetches()) == 1 }, 5*time.Second, 10*time.Millisecond,
		"the outbound tap sees the one fetch")
	x := c.fetches()[0]
	assert.Equal(t, http.MethodGet, x.Method)
	assert.Equal(t, "/repos/acme/widget/tags", x.Path)
	assert.Equal(t, http.StatusOK, x.Status)
	assert.False(t, x.Started.IsZero())

	payload, err := json.Marshal(map[string]any{
		"ref":      "v2",
		"ref_type": "tag",
		"repository": map[string]any{
			"name":      "widget",
			"full_name": "acme/widget",
			"pushed_at": "2026-01-02T03:04:05Z",
			"owner":     map[string]any{"login": "acme"},
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, e.EventsPath(), bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "create")
	req.Header.Set("X-Hub-Signature-256", sign(payload))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, "invalidated", w.Header().Get("X-Mirror-Disposition"))
	assert.Equal(t, DispositionDelivery, c.lastRequest(t).Disposition)

	third := get(t, e, "/repos/acme/widget/tags")
	require.Equal(t, http.StatusOK, third.Code)
	assert.Equal(t, DispositionMiss, c.lastRequest(t).Disposition, "the delivery dropped the listing")
	assert.EqualValues(t, 2, calls.Load())

	assert.True(t, e.Drain(5*time.Second))
	assert.NoError(t, e.Close())
}

func TestNewFromFile_BuildsTheSameEngine(t *testing.T) {
	t.Setenv("EMBED_TEST_SECRET", testSecret)
	path := filepath.Join(t.TempDir(), "mirror.xml")
	src := strings.Replace(embedSpec, "UPSTREAM", "https://api.example.com", 1)
	require.NoError(t, os.WriteFile(path, []byte(src), 0o600))

	e, err := NewFromFile(context.Background(), path, Options{DB: filepath.Join(t.TempDir(), "file.db")})
	require.NoError(t, err)
	assert.True(t, e.Declares(http.MethodGet, "/repos/acme/widget/tags"))
	assert.True(t, e.Drain(time.Second))
	assert.NoError(t, e.Close())
}

func TestNew_RejectsAnInvalidSpec(t *testing.T) {
	_, err := New(context.Background(), "broken.xml", []byte(`<mirror name="x"><route method="GET" path="/a" resource="none"/></mirror>`),
		Options{DB: filepath.Join(t.TempDir(), "broken.db")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken.xml")
}
