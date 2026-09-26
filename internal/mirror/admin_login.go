package mirror

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sessionLifetime bounds a signed-in operator's session.
const sessionLifetime = 12 * time.Hour

// loginSessionCookie carries a signed-in operator's session.
const loginSessionCookie = "mirror_session"

// loginStateCookie carries the OAuth state across the round trip.
const loginStateCookie = "mirror_login_state"

func buildLogin(n *node) (*Login, error) {
	if err := checkAttrs(n, "authorize", "exchange", "user", "name"); err != nil {
		return nil, err
	}
	l := &Login{Authorize: n.Attr("authorize"), Exchange: n.Attr("exchange"), User: n.Attr("user"), Name: n.Attr("name")}
	for _, child := range n.Children() {
		if err := checkAttrs(child); err != nil {
			return nil, err
		}
		v, err := compileContent(child)
		if err != nil {
			return nil, fmt.Errorf("<login><%s>: %w", child.Name(), err)
		}
		v = strings.TrimSpace(v)
		switch child.Name() {
		case "client-id":
			l.ClientID = v
		case "client-secret":
			l.ClientSecret = v
		case "session-secret":
			l.SessionSecret = v
		case "admins":
			l.Admins = v
		case "base-url":
			l.BaseURL = v
		default:
			return nil, fmt.Errorf("<login>: unexpected child element <%s>", child.Name())
		}
	}
	if l.Authorize == "" || l.Exchange == "" || l.User == "" || l.Name == "" {
		return nil, fmt.Errorf("<login> needs authorize=, exchange=, user= and name=")
	}
	return l, nil
}

// adminLogin is a rendered <login>: the values the flow needs at run time.
type adminLogin struct {
	rule         *Login
	clientID     string
	clientSecret string
	key          []byte
	admins       []string
	baseURL      string
	client       *http.Client
}

// newAdminLogin renders a <login>. A missing client id or secret turns
// sign-in off, loudly, and the token still gates the dashboard.
func newAdminLogin(a *Admin, rule *Login) *adminLogin {
	render := func(what, src string) string {
		v, err := renderString(src, a.engine.vars)
		if err != nil {
			logf("<login> %s: %v", what, err)
		}
		return strings.TrimSpace(v)
	}
	l := &adminLogin{
		rule:         rule,
		clientID:     render("client-id", rule.ClientID),
		clientSecret: render("client-secret", rule.ClientSecret),
		baseURL:      strings.TrimSuffix(render("base-url", rule.BaseURL), "/"),
		client:       observedClient(&http.Client{Timeout: 15 * time.Second}, LaneIdentity, a.engine.tel),
	}
	if l.clientID == "" || l.clientSecret == "" {
		logf("dashboard sign-in is OFF: the <login> client id or secret is empty. The dashboard token still works.")
		return nil
	}
	for _, s := range strings.Split(render("admins", rule.Admins), ",") {
		if s = strings.TrimSpace(s); s != "" {
			l.admins = append(l.admins, s)
		}
	}
	if len(l.admins) == 0 {
		logf("dashboard sign-in names no admins, so nobody can sign in. The dashboard token still works.")
	}
	secret := render("session-secret", rule.SessionSecret)
	if secret == "" {
		// Sessions then end at restart, which is safe and only inconvenient.
		secret = randomHex(32)
		logf("dashboard sign-in has no session secret, so sessions end when the process restarts")
	}
	l.key = []byte(secret)
	return l
}

func (l *adminLogin) isAdmin(login string) bool {
	for _, a := range l.admins {
		if strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}

// sign makes a session value: login, expiry, and an HMAC over both.
func (l *adminLogin) sign(login string, expires time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(login)) + "." + strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// session reads a signed-in operator from the request, or "".
func (l *adminLogin) session(r *http.Request) string {
	c, err := r.Cookie(loginSessionCookie)
	if err != nil {
		return ""
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return ""
	}
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return ""
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !l.isAdmin(string(name)) {
		return ""
	}
	return string(name)
}

func (l *adminLogin) callbackURL(a *Admin, r *http.Request) string {
	base := l.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + a.prefix + "/auth/callback"
}

// start sends the operator to the upstream's consent page.
func (l *adminLogin) start(a *Admin, w http.ResponseWriter, r *http.Request) {
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name: loginStateCookie, Value: state, Path: a.prefix, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: 600,
	})
	q := url.Values{"client_id": {l.clientID}, "redirect_uri": {l.callbackURL(a, r)}, "state": {state}}
	http.Redirect(w, r, l.rule.Authorize+"?"+q.Encode(), http.StatusFound)
}

// callback finishes the flow: exchange the code, ask who the operator is,
// and issue a session only to a listed admin.
func (l *adminLogin) callback(a *Admin, w http.ResponseWriter, r *http.Request) {
	state, err := r.Cookie(loginStateCookie)
	if err != nil || state.Value == "" || subtle.ConstantTimeCompare([]byte(state.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "sign-in state does not match; start again", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "the upstream returned no code", http.StatusBadRequest)
		return
	}
	token, err := l.exchange(r.Context(), code, l.callbackURL(a, r))
	if err != nil {
		logf("dashboard sign-in: exchange: %v", err)
		http.Error(w, "sign-in failed at the code exchange", http.StatusBadGateway)
		return
	}
	login, err := l.who(r.Context(), a, token)
	if err != nil {
		logf("dashboard sign-in: %v", err)
		http.Error(w, "sign-in failed asking who you are", http.StatusBadGateway)
		return
	}
	if !l.isAdmin(login) {
		logf("dashboard sign-in refused %q: not on the admin list", login)
		http.Error(w, login+" is not an admin of this mirror", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginSessionCookie, Value: l.sign(login, time.Now().Add(sessionLifetime)), Path: a.prefix,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionLifetime.Seconds()),
	})
	http.Redirect(w, r, a.prefix+"/", http.StatusFound)
}

func (l *adminLogin) exchange(ctx context.Context, code, redirect string) (string, error) {
	form := url.Values{"client_id": {l.clientID}, "client_secret": {l.clientSecret}, "code": {code}, "redirect_uri": {redirect}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.rule.Exchange, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return "", fmt.Errorf("answer %d is not JSON", resp.StatusCode)
	}
	token := scalarString(lookupPath(doc, "access_token"))
	if token == "" {
		return "", fmt.Errorf("answer %d carries no access_token: %s", resp.StatusCode, scalarString(lookupPath(doc, "error")))
	}
	return token, nil
}

func (l *adminLogin) who(ctx context.Context, a *Admin, token string) (string, error) {
	set := http.Header{}
	set.Set("Authorization", "Bearer "+token)
	ans, err := a.engine.up.CallWith(withLane(ctx, LaneIdentity, "", "dashboard"), http.MethodGet, l.rule.User, a.engine.vars, nil, set, nil)
	if err != nil {
		return "", err
	}
	if ans.Status != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", l.rule.User, ans.Status)
	}
	doc, err := decodeJSON(ans.Body)
	if err != nil {
		return "", err
	}
	login := scalarString(lookupPath(doc, l.rule.Name))
	if login == "" {
		return "", fmt.Errorf("%s names no %s", l.rule.User, l.rule.Name)
	}
	return login, nil
}
