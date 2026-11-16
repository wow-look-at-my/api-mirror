package mirror

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loginEngine(t *testing.T, admins string) *Engine {
	t.Helper()
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.PostForm.Get("code") != "good-code" || r.PostForm.Get("client_secret") != "shh" {
			w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		w.Write([]byte(`{"access_token":"user-token"}`))
	}))
	t.Cleanup(exchange.Close)
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" && r.Header.Get("Authorization") == "Bearer user-token" {
			w.Write([]byte(`{"login":"Alice"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}), func(s *Spec) {
		s.Dashboard.Login = &Login{
			Authorize: "https://upstream.example/authorize", Exchange: exchange.URL, User: "/user", Name: "login",
			ClientID: "cid", ClientSecret: "shh", SessionSecret: "session-key", Admins: admins,
		}
	})
	return e
}

// signIn walks the flow and returns the callback's answer.
func walkLogin(t *testing.T, e *Engine, code string) *httptest.ResponseRecorder {
	t.Helper()
	start := httptest.NewRecorder()
	e.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/_mirror/login", nil))
	require.Equal(t, http.StatusFound, start.Code)
	loc, err := url.Parse(start.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "cid", loc.Query().Get("client_id"))
	state := loc.Query().Get("state")
	require.NotEmpty(t, state)

	req := httptest.NewRequest(http.MethodGet, "/_mirror/auth/callback?code="+code+"&state="+state, nil)
	for _, c := range start.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestDashboardSignInIssuesASessionToAnAdmin(t *testing.T) {
	e := loginEngine(t, "bob, alice")

	bare := httptest.NewRecorder()
	e.ServeHTTP(bare, httptest.NewRequest(http.MethodGet, "/_mirror/", nil))
	assert.Equal(t, http.StatusFound, bare.Code, "a browser without a session is sent to sign in")
	assert.Equal(t, "/_mirror/login", bare.Header().Get("Location"))

	done := walkLogin(t, e, "good-code")
	require.Equal(t, http.StatusFound, done.Code, done.Body.String())

	req := httptest.NewRequest(http.MethodGet, "/_mirror/api/overview", nil)
	for _, c := range done.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "the session opens the dashboard")

	forged := httptest.NewRequest(http.MethodGet, "/_mirror/api/overview", nil)
	// The last signature digit always changes. A replace of one letter is a no-op when the value lacks it.
	value := done.Result().Cookies()[0].Value
	digit := "0"
	if strings.HasSuffix(value, "0") {
		digit = "1"
	}
	forged.AddCookie(&http.Cookie{Name: loginSessionCookie, Value: value[:len(value)-1] + digit})
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, forged)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a tampered session is refused")
}

func TestDashboardSignInRefusesSomeoneNotOnTheList(t *testing.T) {
	e := loginEngine(t, "bob")
	assert.Equal(t, http.StatusForbidden, walkLogin(t, e, "good-code").Code)
}

func TestDashboardSignInRefusesABadCodeAndABadState(t *testing.T) {
	e := loginEngine(t, "alice")
	assert.Equal(t, http.StatusBadGateway, walkLogin(t, e, "bad-code").Code)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_mirror/auth/callback?code=good-code&state=forged", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a callback with no matching state cookie is refused")
}

func TestDashboardSignInIsOffWithoutAClient(t *testing.T) {
	e := loginEngine(t, "alice")
	var err error
	e.admin, err = NewAdmin(e)
	require.NoError(t, err)
	e.spec.Dashboard.Login.ClientID = ""
	e.admin, err = NewAdmin(e)
	require.NoError(t, err)
	assert.Nil(t, e.admin.login, "no client means no sign-in, and the token still gates the dashboard")
}
