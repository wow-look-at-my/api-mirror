package mirror

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
)

// SignIn lets a human reach the dashboard through the upstream's own OAuth,
// instead of a shared token pasted into a URL. Every value is template
// source. An admin sees everything; anyone else signed in sees only what
// their own principal has proven.
type SignIn struct {
	ClientID     string
	ClientSecret string
	// Authorize and Exchange are the provider's OAuth endpoints, absolute.
	Authorize string
	Exchange  string
	// User is the upstream path answering who the signed-in token belongs to,
	// and Login the path into that answer naming them.
	User  string
	Login string
	// Admins is a comma-separated list of logins, compared case-insensitively.
	Admins string
	// Secret signs the session cookie. Empty mints a single per process,
	// which signs everybody out on restart.
	Secret string
	// BaseURL is the public origin the provider redirects back to. Empty
	// derives it from the request.
	BaseURL string
}

const (
	sessionName  = "mirror_session"
	stateName    = "mirror_oauth_state"
	sessionLife  = 12 * time.Hour
	stateLife    = 10 * time.Minute
	signInLimit  = 10 * time.Second
	maxSignInRes = 1 << 20
)

// signIn is the resolved rule plus what the sign-in flow keeps.
type signIn struct {
	rule         SignIn
	clientID     string
	clientSecret string
	admins       set.Set[string]
	secret       []byte
	baseURL      string
	client       *http.Client
}

func newSignIn(e *Engine) (*signIn, error) {
	rule := e.spec.Dashboard.SignIn
	if rule == nil {
		return nil, nil
	}
	render := func(what, src string) (string, error) {
		v, err := renderString(src, map[string]any{"env": envMap(), "var": e.vars})
		if err != nil {
			return "", fmt.Errorf("<sign-in> %s: %w", what, err)
		}
		return strings.TrimSpace(v), nil
	}
	s := &signIn{rule: *rule, admins: set.New[string]()}
	var err error
	if s.clientID, err = render("client-id", rule.ClientID); err != nil {
		return nil, err
	}
	if s.clientSecret, err = render("client-secret", rule.ClientSecret); err != nil {
		return nil, err
	}
	if s.clientID == "" || s.clientSecret == "" {
		logf("<sign-in>: client id or secret resolved to nothing -- sign-in is off, the token still opens the dashboard")
		return nil, nil
	}
	admins, err := render("admins", rule.Admins)
	if err != nil {
		return nil, err
	}
	for _, a := range strings.Split(admins, ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			s.admins.Add(a)
		}
	}
	secret, err := render("secret", rule.Secret)
	if err != nil {
		return nil, err
	}
	if secret == "" {
		logf("<sign-in>: no session secret -- minting one, so every session ends on restart")
		secret = randomHex(32)
	}
	s.secret = []byte(secret)
	if s.baseURL, err = render("base-url", rule.BaseURL); err != nil {
		return nil, err
	}
	s.client = observedClient(&http.Client{Timeout: signInLimit}, LaneAdmin, e.tel)
	return s, nil
}

// session is who a signed cookie says is looking.
type session struct {
	Login     string
	Principal string
	Admin     bool
}

func (s *signIn) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verify returns the payload a value signs, or false for anything forged,
// truncated or expired.
func (s *signIn) verify(value string, now time.Time) ([]string, bool) {
	i := strings.LastIndexByte(value, '.')
	if i < 0 {
		return nil, false
	}
	payload := value[:i]
	if !hmac.Equal([]byte(s.sign(payload)), []byte(value)) {
		return nil, false
	}
	parts := strings.Split(payload, "|")
	expires, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || now.Unix() > expires {
		return nil, false
	}
	return parts[:len(parts)-1], true
}

// sessionOf reads the caller's session. The admin bit is decided now, from
// the current list, so removing a login takes effect without a sign-out.
func (s *signIn) sessionOf(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionName)
	if err != nil {
		return session{}, false
	}
	parts, ok := s.verify(c.Value, time.Now())
	if !ok || len(parts) != 2 {
		return session{}, false
	}
	return session{Login: parts[0], Principal: parts[1], Admin: s.admins.Contains(strings.ToLower(parts[0]))}, true
}

func (s *signIn) callbackURL(a *Admin, r *http.Request) string {
	base := s.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return strings.TrimSuffix(base, "/") + a.prefix + "/auth/callback"
}

func (a *Admin) cookie(w http.ResponseWriter, r *http.Request, name, value string, life time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     a.prefix,
		MaxAge:   int(life.Seconds()),
		HttpOnly: true,
		// Lax, not strict: the provider's redirect back is a cross-site
		// navigation, and a strict cookie would not ride it.
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

// login sends the browser to the provider with a state bound to this browser.
func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	s := a.signIn
	state := randomHex(16)
	a.cookie(w, r, stateName, s.sign(state+"|"+strconv.FormatInt(time.Now().Add(stateLife).Unix(), 10)), stateLife)
	q := url.Values{"client_id": {s.clientID}, "redirect_uri": {s.callbackURL(a, r)}, "state": {state}}
	http.Redirect(w, r, s.rule.Authorize+"?"+q.Encode(), http.StatusFound)
}

// callback finishes a sign-in.
func (a *Admin) callback(w http.ResponseWriter, r *http.Request) {
	s := a.signIn
	c, err := r.Cookie(stateName)
	if err != nil {
		http.Error(w, "sign-in: no state cookie; start again from the dashboard", http.StatusBadRequest)
		return
	}
	parts, ok := s.verify(c.Value, time.Now())
	if !ok || len(parts) != 1 || !hmac.Equal([]byte(parts[0]), []byte(r.URL.Query().Get("state"))) {
		http.Error(w, "sign-in: the state does not match this browser", http.StatusBadRequest)
		return
	}
	a.cookie(w, r, stateName, "", -time.Second)
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "sign-in: the provider sent no code", http.StatusBadRequest)
		return
	}
	ctx := withLane(r.Context(), LaneAdmin, "", "sign-in")
	token, err := s.exchange(ctx, code, s.callbackURL(a, r))
	if err != nil {
		http.Error(w, "sign-in: "+err.Error(), http.StatusBadRequest)
		return
	}
	login, principal, err := a.whoIs(ctx, token)
	if err != nil {
		http.Error(w, "sign-in: "+err.Error(), http.StatusBadRequest)
		return
	}
	expires := strconv.FormatInt(time.Now().Add(sessionLife).Unix(), 10)
	a.cookie(w, r, sessionName, s.sign(login+"|"+principal+"|"+expires), sessionLife)
	http.Redirect(w, r, a.prefix+"/", http.StatusFound)
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	a.cookie(w, r, sessionName, "", -time.Second)
	http.Redirect(w, r, a.prefix+"/", http.StatusFound)
}

// exchange trades the code for the signed-in user's token.
func (s *signIn) exchange(ctx context.Context, code, redirect string) (string, error) {
	form := url.Values{"client_id": {s.clientID}, "client_secret": {s.clientSecret}, "code": {code}, "redirect_uri": {redirect}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rule.Exchange, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange the code: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSignInRes))
	if err != nil {
		return "", fmt.Errorf("exchange the code: %w", err)
	}
	var answer struct {
		Token string `json:"access_token"`
		Error string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("exchange answered %d, not JSON", resp.StatusCode)
	}
	if answer.Token == "" {
		return "", fmt.Errorf("exchange answered %d with no token: %s", resp.StatusCode, answer.Error)
	}
	return answer.Token, nil
}

// whoIs names the signed-in token's login and the principal the reveal layer
// knows it by, so the self view shows the grants this person earned.
func (a *Admin) whoIs(ctx context.Context, token string) (string, string, error) {
	e := a.engine
	h := http.Header{"Authorization": {"Bearer " + token}}
	answer, err := e.up.Call(ctx, http.MethodGet, a.signIn.rule.User, e.vars, h, nil)
	if err != nil {
		return "", "", fmt.Errorf("ask who signed in: %w", err)
	}
	if answer.Status != http.StatusOK {
		return "", "", fmt.Errorf("%s answered %d", a.signIn.rule.User, answer.Status)
	}
	doc, err := decodeJSON(answer.Body)
	if err != nil {
		return "", "", err
	}
	login := fmt.Sprint(lookupPath(doc, a.signIn.rule.Login))
	if login == "" || login == "<nil>" {
		return "", "", fmt.Errorf("%s carries no %s", a.signIn.rule.User, a.signIn.rule.Login)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return "", "", err
	}
	req.Header = h
	principal, refusal := e.ids.resolve(ctx, req)
	if refusal != nil {
		return "", "", fmt.Errorf("resolve the principal: %s", refusal.message)
	}
	return login, principal, nil
}

// WhoAmI is what the page asks to know which tabs to draw.
type WhoAmI struct {
	Login     string `json:"login,omitempty"`
	Principal string `json:"principal,omitempty"`
	Admin     bool   `json:"admin"`
	SignIn    bool   `json:"sign_in"`
}

func (a *Admin) whoami(w http.ResponseWriter, r *http.Request) {
	view := WhoAmI{Admin: true, SignIn: a.signIn != nil}
	if s, ok := a.sessionFrom(r); ok && !a.authorized(r) {
		view = WhoAmI{Login: s.Login, Principal: s.Principal, Admin: s.Admin, SignIn: true}
	}
	writeJSON(w, http.StatusOK, view)
}

// me is the self view: what the signed-in principal has proven.
func (a *Admin) me(w http.ResponseWriter, r *http.Request) {
	s, ok := a.sessionFrom(r)
	if !ok {
		http.Error(w, "sign in to see your own standing", http.StatusForbidden)
		return
	}
	standing, err := a.engine.store.StandingOf(r.Context(), s.Principal, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, standing)
}

func (a *Admin) sessionFrom(r *http.Request) (session, bool) {
	if a.signIn == nil {
		return session{}, false
	}
	return a.signIn.sessionOf(r)
}

// selfPaths are what a signed-in non-admin may reach: the page, its assets,
// who they are, and their own standing.
func (a *Admin) selfPath(path string) bool {
	switch strings.TrimPrefix(path, a.prefix) {
	case "/", "/app.js", "/style.css", "/api/whoami", "/api/me", "/auth/logout":
		return true
	}
	return false
}
