package mirror

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// defaultDashboardPath is where the surface lives when a spec names nowhere.
const defaultDashboardPath = "/_mirror"

// Admin routes the dashboard, its JSON, and the subscription API.
type Admin struct {
	engine *Engine
	prefix string
	token  string
	// minted means generated for this process, so the startup log carries the URL.
	minted bool
	mux    *http.ServeMux
}

// NewAdmin builds the operator surface, always. A spec chooses where it lives
// and what gates it, never whether it exists: an optional view is nobody
// has when they need it.
func NewAdmin(e *Engine) *Admin {
	d := e.spec.Dashboard
	prefix := strings.TrimSuffix(d.Path, "/")
	if prefix == "" {
		prefix = defaultDashboardPath
	}
	a := &Admin{engine: e, prefix: prefix, mux: http.NewServeMux()}

	token, err := renderString(d.Token, e.vars)
	if err != nil {
		logf("<dashboard> token: %v", err)
	}
	a.token = strings.TrimSpace(token)
	if a.token == "" {
		// On by default, never OPEN by default: the prefix is guessable.
		a.token = randomHex(16)
		a.minted = true
	}
	a.routes()
	return a
}

// URL is the address that opens the dashboard, carrying a minted token.
func (a *Admin) URL(addr string) string {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	url := "http://" + host + a.prefix + "/"
	if a.minted {
		return url + "?token=" + a.token
	}
	return url
}

// Minted reports whether the token was generated rather than declared.
func (a *Admin) Minted() bool { return a != nil && a.minted }

// Prefix is where the surface lives.
func (a *Admin) Prefix() string { return a.prefix }

// Handles reports whether this path belongs to the operator surface.
func (a *Admin) Handles(path string) bool {
	if a == nil {
		return false
	}
	return path == a.prefix || strings.HasPrefix(path, a.prefix+"/")
}

func (a *Admin) routes() {
	p := a.prefix
	a.mux.HandleFunc("GET "+p+"/", a.page)
	a.mux.HandleFunc("GET "+p+"/app.js", a.asset("app.js", "text/javascript; charset=utf-8"))
	a.mux.HandleFunc("GET "+p+"/style.css", a.asset("style.css", "text/css; charset=utf-8"))
	a.mux.HandleFunc("GET "+p+"/api/overview", a.overview)
	a.mux.HandleFunc("GET "+p+"/api/requests", a.requests)
	a.mux.HandleFunc("GET "+p+"/api/timeline", a.timeline)
	a.mux.HandleFunc("GET "+p+"/api/rates", a.rates)
	a.mux.HandleFunc("GET "+p+"/api/brief", a.brief)
	a.mux.HandleFunc("GET "+p+"/api/principals", a.principals)
	a.mux.HandleFunc("GET "+p+"/api/resources", a.resources)
	a.mux.HandleFunc("GET "+p+"/api/events", a.events)
	a.mux.HandleFunc("GET "+p+"/api/spec", a.specReport)
	a.mux.HandleFunc("GET "+p+"/api/subscriptions", a.listSubscriptions)
	a.mux.HandleFunc("POST "+p+"/api/subscriptions", a.createSubscription)
	a.mux.HandleFunc("DELETE "+p+"/api/subscriptions/{id}", a.deleteSubscription)
	a.mux.HandleFunc("POST "+p+"/api/refresh", a.runRefresh)
	a.mux.HandleFunc("GET "+p+"/api/check", a.check)
	a.mux.HandleFunc("POST "+p+"/api/check", a.check)
}

// ServeHTTP gates the surface and dispatches.
func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		// A mistyped token gets the same answer as a probe for the prefix.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	a.keepToken(w, r)
	if r.URL.Path == a.prefix {
		http.Redirect(w, r, a.prefix+"/", http.StatusMovedPermanently)
		return
	}
	a.engine.setCORS(w, r)
	a.mux.ServeHTTP(w, r)
}

// sessionCookie carries the token for a BROWSER's own subresource fetches.
const sessionCookie = "mirror_admin"

// keepToken hands the browser back the token the caller just proved. Strict
// same-site keeps it off a cross-site request.
func (a *Admin) keepToken(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value == a.token {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.token,
		Path:     a.prefix,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

// authorized checks the token in constant time. carriers: a human opens a
// URL, the page fetches with a header, a script uses what it already has, and
// the browser sends back the cookie for a subresource it fetches itself.
func (a *Admin) authorized(r *http.Request) bool {
	presented := r.URL.Query().Get("token")
	if presented == "" {
		presented = r.Header.Get("X-Mirror-Token")
	}
	if presented == "" {
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if presented == "" {
		if c, err := r.Cookie(sessionCookie); err == nil {
			presented = c.Value
		}
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) == 1
}

// Overview is the dashboard's front page: what this mirror is and what it has
// been doing.
type Overview struct {
	Mirror        string          `json:"mirror"`
	Title         string          `json:"title"`
	Upstream      string          `json:"upstream"`
	Fingerprint   string          `json:"fingerprint"`
	Started       time.Time       `json:"started"`
	Resources     int             `json:"resources"`
	Routes        int             `json:"routes"`
	Events        int             `json:"events"`
	Kinds         []KindStat      `json:"kinds"`
	Lanes         []LaneTally     `json:"lanes"`
	Requests      []*RequestGroup `json:"groups"`
	Timeline      TimelineStats   `json:"timeline"`
	Refresh       RefreshStats    `json:"refresh"`
	Replay        ReplayStats     `json:"replay"`
	Notify        NotifyStats     `json:"notify"`
	Deliveries    DeliveryStats   `json:"deliveries"`
	Principals    int             `json:"principals"`
	Denials       int64           `json:"denials"`
	UpstreamBytes int             `json:"upstream_bytes"`
	// Passthrough is what the spec still does not model: how finished this is.
	Passthrough int `json:"passthrough"`
	Answered    int `json:"answered"`
}

func (a *Admin) overview(w http.ResponseWriter, r *http.Request) {
	e := a.engine
	ctx := r.Context()
	now := time.Now()

	ov := Overview{
		Mirror:        e.spec.Name,
		Title:         e.spec.Dashboard.Title,
		Upstream:      e.up.base,
		Fingerprint:   e.spec.Fingerprint(),
		Started:       e.tel.Started,
		Resources:     len(e.spec.Resources),
		Routes:        len(e.spec.Routes),
		Lanes:         e.tel.Lanes(),
		Requests:      e.tel.Requests.Groups(),
		Timeline:      e.tel.Timeline.Stats(),
		Refresh:       e.refresh.Stats(),
		Replay:        e.replay.Stats(),
		Notify:        e.notify.Stats(ctx),
		Deliveries:    e.ingest.Stats(),
		UpstreamBytes: e.tel.UpstreamBytes(),
	}
	if ov.Title == "" {
		ov.Title = e.spec.Name
	}
	if e.spec.Events != nil {
		ov.Events = len(e.spec.Events.List)
	}
	for _, g := range ov.Requests {
		ov.Passthrough += g.Dispositions[DispPassthrough]
		ov.Answered += g.Dispositions[DispHit] + g.Dispositions[DispMiss]
	}
	if kinds, err := e.store.KindStats(ctx); err != nil {
		logf("dashboard: kind stats: %v", err)
	} else {
		ov.Kinds = kinds
	}
	if ps, err := e.store.Principals(ctx, now); err != nil {
		logf("dashboard: principals: %v", err)
	} else {
		ov.Principals = len(ps)
	}
	if n, err := e.store.CountDenials(ctx, now); err != nil {
		logf("dashboard: denials: %v", err)
	} else {
		ov.Denials = n
	}
	writeJSON(w, http.StatusOK, ov)
}
