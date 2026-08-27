package mirror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operator surface, and the two properties that make it worth having: it is
// there without being asked for, and it is not open to whoever guesses the
// prefix.

// adminGet drives one dashboard request, carrying the token the surface minted.
func adminGet(t *testing.T, e *Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Mirror-Token", e.admin.token)
	e.ServeHTTP(rec, r)
	return rec
}

func adminJSON[T any](t *testing.T, e *Engine, path string) T {
	t.Helper()
	rec := adminGet(t, e, path)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestDashboardIsOnWithoutBeingAskedFor(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	require.NotNil(t, e.admin, "a mirror nobody can see into is a mirror nobody can operate")
	assert.Equal(t, defaultDashboardPath, e.admin.Prefix())
	assert.True(t, e.admin.Minted(), "a spec that named no token gets one minted for this process")

	rec := adminGet(t, e, defaultDashboardPath+"/")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "app.js")
}

func TestDashboardRefusesWithoutTheToken(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, defaultDashboardPath+"/api/overview", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"on by default is not the same as open by default: this page carries every path and principal the mirror has seen")
}

func TestDashboardTakesTheTokenEveryWayACallerCarriesIt(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	token := e.admin.token

	for _, tc := range []struct {
		name string
		wire func(*http.Request)
	}{
		{"a human opening a url", func(r *http.Request) { r.URL.RawQuery = "token=" + token }},
		{"the page fetching its own json", func(r *http.Request) { r.Header.Set("X-Mirror-Token", token) }},
		{"a script with a bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }},
		{"a browser sending back the cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, defaultDashboardPath+"/api/overview", nil)
			tc.wire(r)
			e.ServeHTTP(rec, r)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

func TestDashboardServesItsOwnAssets(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	for path, wantType := range map[string]string{
		"/app.js":    "text/javascript",
		"/style.css": "text/css",
	} {
		rec := adminGet(t, e, defaultDashboardPath+path)
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Contains(t, rec.Header().Get("Content-Type"), wantType)
		assert.NotEmpty(t, rec.Body.Bytes())
	}
}

// A browser fetches a stylesheet and a module ITSELF, with no header and no
// query string, so the two assets were refused and the page loaded unstyled
// and inert. Every existing asset test passed, because each wired the token
// the way the PAGE does. This one is the sequence a browser performs.
func TestABrowserCanLoadTheAssetsTheShellAsksFor(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())

	shell := httptest.NewRecorder()
	e.ServeHTTP(shell, httptest.NewRequest(http.MethodGet,
		defaultDashboardPath+"/?token="+e.admin.token, nil))
	require.Equal(t, http.StatusOK, shell.Code)

	jar := shell.Result().Cookies()
	require.NotEmpty(t, jar, "the shell handed the browser nothing to fetch its own files with")

	for path, wantType := range map[string]string{
		"/app.js":    "text/javascript",
		"/style.css": "text/css",
	} {
		// No header, no query: exactly what <script src> and <link href> send.
		r := httptest.NewRequest(http.MethodGet, defaultDashboardPath+path, nil)
		for _, c := range jar {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, r)
		require.Equalf(t, http.StatusOK, rec.Code, "%s: an unstyled, inert page is what a 401 here looks like", path)
		assert.Contains(t, rec.Header().Get("Content-Type"), wantType)
	}

	// The cookie is the only thing that opened those; without it they stay shut.
	bare := httptest.NewRecorder()
	e.ServeHTTP(bare, httptest.NewRequest(http.MethodGet, defaultDashboardPath+"/app.js", nil))
	assert.Equal(t, http.StatusUnauthorized, bare.Code)
}

func TestOverviewCountsWhatIsAnsweredAgainstWhatStillLeaves(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}))
	require.Equal(t, http.StatusOK, get(t, e, "/widgets/7").Code)
	get(t, e, "/nothing/declares/this")

	ov := adminJSON[Overview](t, e, defaultDashboardPath+"/api/overview")
	assert.Equal(t, 1, ov.Answered)
	assert.Equal(t, 1, ov.Passthrough,
		"the share of traffic still leaving is the one number that says how finished this mirror is")
	assert.NotEmpty(t, ov.Fingerprint)
	assert.NotEmpty(t, ov.Kinds)
}

func TestRequestLogSeesTheAdminSurfaceToo(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	adminGet(t, e, defaultDashboardPath+"/api/overview")

	found := false
	for _, g := range e.tel.Requests.Groups() {
		if g.Dispositions[DispAdmin] > 0 {
			found = true
		}
	}
	assert.True(t, found, "the dashboard's own traffic is traffic")
}

func TestBriefTurnsAPassthroughIntoSomethingToDo(t *testing.T) {
	// 404 is fine: a passthrough is tallied by what it WAS, not what came back.
	e, _ := newTestEngine(t, http.NotFoundHandler())

	get(t, e, "/widgets/7/releases")
	get(t, e, "/widgets/9/releases")

	view := adminJSON[BriefView](t, e, defaultDashboardPath+"/api/brief")
	require.NotEmpty(t, view.Items)
	item := view.Items[0]
	assert.Equal(t, 2, item.Count)
	assert.Equal(t, "/widgets/{id}/releases", item.Shape,
		"the trailing collection name is what says which family this is")
	assert.Contains(t, item.Reasons, string(PassUnrouted))
	assert.Contains(t, item.Sketch, "<route", "a shape with no declaration beside it is a diagnosis, not a task")
	assert.Contains(t, item.Sketch, `<key name="widget"`)
}

func TestResourcesTabDescribesTheGateAsWellAsTheColumns(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	views := adminJSON[[]ResourceView](t, e, defaultDashboardPath+"/api/resources?resource=widget")

	var widget ResourceView
	for _, v := range views {
		if v.Name == "widget" {
			widget = v
		}
	}
	require.Equal(t, "widget", widget.Name)
	assert.Equal(t, []string{"id"}, widget.Keys)
	assert.Len(t, widget.Fields, 4)
	assert.Contains(t, widget.Routes, "GET /widgets/{id}")
	assert.Equal(t, "true", widget.Reveal.Public)
}

func TestRatesTabSaysWhenTheSpecNamedNoHeaders(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	view := adminJSON[RatesView](t, e, defaultDashboardPath+"/api/rates")
	assert.False(t, view.Declared,
		"an empty table must not mean 'no traffic yet' and 'cannot read a budget' at the same time")
}

func TestEventsTabListsDeclaredTypesEvenAtZero(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Events = &Events{
			Path:            "/hook",
			Secret:          "shh",
			SignatureHeader: "X-Hub-Signature-256",
			TypeHeader:      "X-Event",
			List: []*Event{{
				Type: "widget.changed", Resource: "widget",
				Subject: "widget:{{ .payload.id }}", Clock: "updated_at",
				Sets: []Set{{Field: "title", From: "title"}},
			}},
		}
	})
	ingest, err := NewIngest(e.spec, e.store, e.vars)
	require.NoError(t, err)
	ingest.SetTelemetry(e.tel)
	e.SetIngest(ingest)

	view := adminJSON[EventsView](t, e, defaultDashboardPath+"/api/events")
	require.Len(t, view.Declared, 1)
	assert.Equal(t, "widget.changed", view.Declared[0].Type)
	assert.Equal(t, "updated_at", view.Declared[0].Clock)
	assert.Contains(t, view.Stats.Declared, "widget.changed",
		"a type the provider was never subscribed to looks exactly like a quiet week without this")
	assert.False(t, view.Replay.Enabled)
}

func TestSpecTabPrintsWhatTheSpecDerives(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	view := adminJSON[SpecView](t, e, defaultDashboardPath+"/api/spec")
	assert.NotEmpty(t, view.Fingerprint)
	assert.Contains(t, view.DDL, "CREATE TABLE")
	assert.Contains(t, view.DDL, "widget")
}

func TestPrincipalsTabReportsWhoHasProvenWhat(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	view := adminJSON[PrincipalsView](t, e, defaultDashboardPath+"/api/principals")
	assert.Empty(t, view.Principals)
	assert.Zero(t, view.Denials)
}

func TestSubscriptionsTabSaysSoWhenNoNotifyIsDeclared(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())
	rec := adminGet(t, e, defaultDashboardPath+"/api/subscriptions")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestHealthAnswersOutsideTheDashboardToken(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Health = &Health{Live: "/.well-known/live", PreUpdate: "/.well-known/pre-update"}
	})
	// A checker has no credential to give, and an unregistered path here does
	// not 404 -- it falls through to the proxy and answers whatever the upstream
	// says about it.
	for _, path := range []string{"/.well-known/live", "/.well-known/pre-update"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equalf(t, http.StatusOK, rec.Code, "%s answered %d", path, rec.Code)
	}
}

func TestCORSExposesTheMirrorsOwnHeaders(t *testing.T) {
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"7","title":"hi"}`))
	}), func(s *Spec) {
		s.CORS = &CORS{Origins: []string{"*"}, Expose: append([]string{}, defaultExposed...)}
	})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/widgets/7", nil)
	r.Header.Set("Origin", "https://viewer.example")
	e.ServeHTTP(rec, r)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "X-Mirror-Cache",
		"without this a browser client cannot tell a hit from a passthrough")
}

func TestCORSPreflightIsAnsweredWithoutAuthentication(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.CORS = &CORS{Origins: []string{"https://viewer.example"}, MaxAge: 10 * time.Minute}
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/widgets/7", nil)
	r.Header.Set("Origin", "https://viewer.example")
	r.Header.Set("Access-Control-Request-Method", "GET")
	r.Header.Set("Access-Control-Request-Headers", "authorization")
	e.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"a preflight carries no credential by definition, so requiring one refuses every cross-origin request")
	assert.Equal(t, "https://viewer.example", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "authorization", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
}

func TestAdminURLCarriesAMintedTokenAndNotADeclaredOne(t *testing.T) {
	minted, _ := newTestEngine(t, http.NotFoundHandler())
	assert.Contains(t, minted.admin.URL(":8080"), "token=",
		"a token nobody was told is a dashboard nobody can open")
	assert.True(t, strings.HasPrefix(minted.admin.URL(":8080"), "http://localhost:8080"))

	declared, _ := newTestEngine(t, http.NotFoundHandler(), func(s *Spec) {
		s.Dashboard.Token = "operator-chose-this"
	})
	assert.False(t, declared.admin.Minted())
	assert.NotContains(t, declared.admin.URL(":8080"), "token=",
		"a declared token is the operator's to distribute, not ours to print")
}
