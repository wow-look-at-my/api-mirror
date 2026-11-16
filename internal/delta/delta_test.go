package delta

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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

// fakeAPI answers every path with body, and counts the requests by method.
type fakeAPI struct {
	*httptest.Server
	gets, posts atomic.Int32
	lastAuth    atomic.Value
}

func newFakeAPI(t *testing.T, status int, body func(base, path string) string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.posts.Add(1)
		} else {
			f.gets.Add(1)
		}
		f.lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", time.Now().String())
		w.Header().Set("X-Mirror-Cache", "hit")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body("http://"+r.Host, r.URL.Path))
	}))
	t.Cleanup(f.Close)
	return f
}

func newDelta(t *testing.T, cfg *Config) (*Delta, *Reporter) {
	t.Helper()
	require.NoError(t, cfg.Validate())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep, err := NewReporter(log, cfg.Report, cfg.Recent, cfg.Ignore)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rep.Close() })
	return New(cfg, rep, log), rep
}

func TestProxyServesTruthAndReportsMirrorDifferences(t *testing.T) {
	truth := newFakeAPI(t, 200, func(base, path string) string {
		return string(mustJSON(map[string]any{
			"name": "demo", "url": base + path, "stars": 5,
			"owner": map[string]any{"login": "o", "html_url": base + "/o"},
		}))
	})
	mirror := newFakeAPI(t, 200, func(base, path string) string {
		return string(mustJSON(map[string]any{
			"name": "demo", "url": base + path, "stars": 4,
			"owner": map[string]any{"login": "o"},
		}))
	})
	report := filepath.Join(t.TempDir(), "delta.ndjson")
	d, rep := newDelta(t, &Config{
		Truth:   Target{URL: truth.URL},
		Mirrors: []Target{{Name: "local", URL: mirror.URL}},
		Report:  report,
		Ignore:  []*Rule{{Path: "$..*_url", Kinds: []Kind{KindMissing}, Reason: "the mirror drops URL fields"}},
	})
	srv := httptest.NewServer(d)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/demo?x=1", nil)
	req.Header.Set("Authorization", "token abc")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, TruthName, resp.Header.Get("X-Delta-Served"))
	assert.Contains(t, string(body), `"stars":5`, "the client gets the truth answer")
	require.NoError(t, d.Drain(5*time.Second))

	assert.Equal(t, "token abc", truth.lastAuth.Load())
	assert.Equal(t, "token abc", mirror.lastAuth.Load())

	s := rep.Summary()
	assert.Equal(t, 1, s.Exchanges)
	assert.Equal(t, 1, s.Differing)
	require.Len(t, s.Rows, 1, "the url fields agree once each base is normalized, and html_url is ignored")
	assert.Equal(t, KindChanged, s.Rows[0].Kind)
	assert.Equal(t, "$.stars", s.Rows[0].Shape)
	assert.Equal(t, 1, s.Rules[0].Matched)

	recs := rep.Recent(10)
	require.Len(t, recs, 1)
	assert.Equal(t, "x=1", recs[0].Query)
	assert.Equal(t, "hit", recs[0].Answer.Annotations["X-Mirror-Cache"])
	require.Len(t, recs[0].Ignored, 1)
	assert.Equal(t, "$.owner.html_url", recs[0].Ignored[0].Path)

	f, err := os.Open(report)
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	require.True(t, sc.Scan())
	var line Record
	require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
	assert.Equal(t, "/repos/o/demo", line.Path)
	require.Len(t, line.Diffs, 1)
	assert.Equal(t, "$.stars", line.Diffs[0].Path)
	assert.False(t, sc.Scan(), "one request against one mirror is one record")
}

func TestProxyForwardsWritesToTheServedTargetOnly(t *testing.T) {
	truth := newFakeAPI(t, 201, func(string, string) string { return `{}` })
	mirror := newFakeAPI(t, 201, func(string, string) string { return `{}` })
	d, rep := newDelta(t, &Config{Truth: Target{URL: truth.URL}, Mirrors: []Target{{Name: "m", URL: mirror.URL}}})
	srv := httptest.NewServer(d)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/repos/o/r/issues", "application/json", strings.NewReader(`{"title":"x"}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.NoError(t, d.Drain(5*time.Second))
	assert.Equal(t, 201, resp.StatusCode)
	assert.Equal(t, int32(1), truth.posts.Load())
	assert.Equal(t, int32(0), mirror.posts.Load(), "a write that fans out runs once per target")
	assert.Equal(t, 1, rep.Summary().Uncompared)
}

func TestProxyComparesAConfiguredPost(t *testing.T) {
	truth := newFakeAPI(t, 200, func(string, string) string { return `{"data":{"n":1}}` })
	mirror := newFakeAPI(t, 200, func(string, string) string { return `{"data":{"n":2}}` })
	d, rep := newDelta(t, &Config{
		Truth:   Target{URL: truth.URL},
		Mirrors: []Target{{Name: "m", URL: mirror.URL}},
		Compare: []Match{{Method: "POST", Route: "^/graphql$"}},
		Serve:   "m",
	})
	srv := httptest.NewServer(d)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(`{"query":"{n}"}`))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, d.Drain(5*time.Second))
	assert.Equal(t, "m", resp.Header.Get("X-Delta-Served"))
	assert.JSONEq(t, `{"data":{"n":2}}`, string(body))
	s := rep.Summary()
	require.Len(t, s.Rows, 1)
	assert.Equal(t, "$.data.n", s.Rows[0].Shape)
}

func TestProxyReportsAMirrorThatDoesNotAnswer(t *testing.T) {
	truth := newFakeAPI(t, 200, func(string, string) string { return `{}` })
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	d, rep := newDelta(t, &Config{Truth: Target{URL: truth.URL}, Mirrors: []Target{{Name: "dead", URL: dead.URL}}})

	recs := d.Compare(context.Background(), &Inbound{Method: "GET", Path: "/x", Header: http.Header{}})
	require.Len(t, recs, 1)
	require.Len(t, recs[0].Diffs, 1)
	assert.Equal(t, KindError, recs[0].Diffs[0].Kind)
	assert.NotEmpty(t, recs[0].Answer.Error)
	assert.Equal(t, 1, rep.Summary().Errors)
}

func TestProxyAnswers502WhenTheServedTargetFails(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	mirror := newFakeAPI(t, 200, func(string, string) string { return `{}` })
	d, _ := newDelta(t, &Config{Truth: Target{URL: dead.URL}, Mirrors: []Target{{Name: "m", URL: mirror.URL}}})
	srv := httptest.NewServer(d)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	require.NoError(t, err)
	resp.Body.Close()
	require.NoError(t, d.Drain(5*time.Second))
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestTargetHeadersReplaceTheClients(t *testing.T) {
	truth := newFakeAPI(t, 200, func(string, string) string { return `{}` })
	mirror := newFakeAPI(t, 200, func(string, string) string { return `{}` })
	d, _ := newDelta(t, &Config{
		Truth:   Target{URL: truth.URL, Headers: map[string]string{"Authorization": "token truth"}},
		Mirrors: []Target{{Name: "m", URL: mirror.URL}},
	})
	d.Compare(context.Background(), &Inbound{Method: "GET", Path: "/x", Header: http.Header{"Authorization": {"token client"}}})
	assert.Equal(t, "token truth", truth.lastAuth.Load())
	assert.Equal(t, "token client", mirror.lastAuth.Load())
}

func TestTargetBasePathIsKept(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.RequestURI())
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer api.Close()
	d, _ := newDelta(t, &Config{Truth: Target{URL: api.URL + "/api/v3/"}, Mirrors: []Target{{Name: "m", URL: api.URL}}})
	d.Compare(context.Background(), &Inbound{Method: "GET", Path: "/repos/a%2Fb", Query: "q=1", Header: http.Header{}})
	assert.ElementsMatch(t, []string{"/api/v3/repos/a%2Fb?q=1", "/repos/a%2Fb?q=1"}, got)
}

func TestAdminEndpoints(t *testing.T) {
	truth := newFakeAPI(t, 200, func(string, string) string { return `{"a":1}` })
	mirror := newFakeAPI(t, 200, func(string, string) string { return `{"a":2}` })
	d, _ := newDelta(t, &Config{Truth: Target{URL: truth.URL}, Mirrors: []Target{{Name: "m", URL: mirror.URL}}})
	for range 3 {
		d.Compare(context.Background(), &Inbound{Method: "GET", Path: "/x", Header: http.Header{}})
	}
	srv := httptest.NewServer(d)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("/_delta/summary")
	assert.Equal(t, 200, code)
	var s Summary
	require.NoError(t, json.Unmarshal([]byte(body), &s))
	assert.Equal(t, 3, s.Differing)
	require.Len(t, s.Rows, 1)
	assert.Equal(t, 3, s.Rows[0].Count)

	code, body = get("/_delta/summary?format=text")
	assert.Equal(t, 200, code)
	assert.Contains(t, body, "3x GET /x [m] changed $.a")

	code, body = get("/_delta/recent?limit=2")
	assert.Equal(t, 200, code)
	var recs []Record
	require.NoError(t, json.Unmarshal([]byte(body), &recs))
	assert.Len(t, recs, 2)

	code, _ = get("/_delta/recent?limit=0")
	assert.Equal(t, 400, code)
	code, _ = get("/_delta/nope")
	assert.Equal(t, 404, code)
	assert.Equal(t, int32(3), truth.gets.Load(), "admin paths never reach a target")
}
