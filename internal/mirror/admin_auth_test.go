package mirror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
)

func TestSignIn_AForgedOrExpiredValueIsRefused(t *testing.T) {
	s := &signIn{secret: []byte("k")}
	now := time.Now()
	good := s.sign("octo|user:1|" + strconv.FormatInt(now.Add(time.Hour).Unix(), 10))

	parts, ok := s.verify(good, now)
	require.True(t, ok)
	assert.Equal(t, []string{"octo", "user:1"}, parts)

	_, ok = s.verify("boss|user:9|"+strconv.FormatInt(now.Add(time.Hour).Unix(), 10)+good[len(good)-65:], now)
	assert.False(t, ok, "a payload the key never signed")
	_, ok = (&signIn{secret: []byte("other")}).verify(good, now)
	assert.False(t, ok, "a value signed with another key")
	_, ok = s.verify(good, now.Add(2*time.Hour))
	assert.False(t, ok, "an expired session")
}

func gatedAdmin(t *testing.T) (*Admin, *signIn) {
	t.Helper()
	s := &signIn{secret: []byte("k"), admins: set.Of("boss")}
	a := &Admin{engine: &Engine{spec: &Spec{}}, prefix: "/_mirror", token: "tok", mux: http.NewServeMux(), signIn: s}
	a.routes()
	return a, s
}

func sessionCookieFor(s *signIn, login string) *http.Cookie {
	return &http.Cookie{Name: sessionName, Value: s.sign(login + "|user:1|" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))}
}

func TestSignIn_ANonAdminReachesOnlyTheirOwnView(t *testing.T) {
	a, s := gatedAdmin(t)
	get := func(path string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}

	someone := sessionCookieFor(s, "someone")
	assert.Equal(t, http.StatusForbidden, get("/_mirror/api/overview", someone).Code)
	w := get("/_mirror/api/whoami", someone)
	require.Equal(t, http.StatusOK, w.Code)
	var who WhoAmI
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &who))
	assert.Equal(t, WhoAmI{Login: "someone", Principal: "user:1", Admin: false, SignIn: true}, who)

	w = get("/_mirror/api/whoami", sessionCookieFor(s, "Boss"))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &who))
	assert.True(t, who.Admin, "the allow-list compares logins case-insensitively")

	assert.Equal(t, http.StatusFound, get("/_mirror/", nil).Code, "a browser with no session is sent to sign in")
	assert.Equal(t, http.StatusUnauthorized, get("/_mirror/api/overview", nil).Code)
}

func TestSignIn_TheRoundTripSignsTheBrowserIn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "the-code", r.PostForm.Get("code"))
			require.Equal(t, "secret", r.PostForm.Get("client_secret"))
			body, err := json.Marshal(map[string]string{"access_token": "gho_person"})
			require.NoError(t, err)
			w.Write(body)
		case "/user":
			require.Equal(t, "Bearer gho_person", r.Header.Get("Authorization"))
			body, err := json.Marshal(map[string]any{"login": "Boss", "id": 7})
			require.NoError(t, err)
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)

	spec := &Spec{Name: "t", Upstream: Upstream{Base: up.URL, Forward: []string{"Authorization"}}}
	spec.Dashboard.SignIn = &SignIn{
		ClientID: "id", ClientSecret: "secret",
		Authorize: "https://provider.invalid/authorize", Exchange: up.URL + "/login/oauth/access_token",
		User: "/user", Login: "login", Admins: "boss", Secret: "k",
	}
	vars := map[string]any{}
	upstream, err := NewUpstreamer(spec, vars, nil)
	require.NoError(t, err)
	e := &Engine{spec: spec, up: upstream, vars: vars, tel: NewTelemetry(RateHeaders{})}
	e.ids = newIdentities(nil, upstream, vars)
	a, err := NewAdmin(e)
	require.NoError(t, err)
	require.NotNil(t, a.signIn)

	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/_mirror/auth/login", nil))
	require.Equal(t, http.StatusFound, w.Code)
	sent, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	state := sent.Query().Get("state")
	require.NotEmpty(t, state)
	stateCookie := w.Result().Cookies()[0]

	r := httptest.NewRequest(http.MethodGet, "/_mirror/auth/callback?code=the-code&state="+state, nil)
	r.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionName && c.Value != "" {
			session = c
		}
	}
	require.NotNil(t, session)

	r = httptest.NewRequest(http.MethodGet, "/_mirror/api/whoami", nil)
	r.AddCookie(session)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	var who WhoAmI
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &who))
	assert.Equal(t, "Boss", who.Login)
	assert.True(t, who.Admin)
	assert.Equal(t, "token:"+fingerprint("Bearer gho_person"), who.Principal)

	r = httptest.NewRequest(http.MethodGet, "/_mirror/auth/callback?code=the-code&state=forged", nil)
	r.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, "a state that is not this browser's is refused")
}
