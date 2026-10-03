package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIssuer is an upstream that issues a token per grant, the way an App
// installation does, and records what each data request carried.
type fakeIssuer struct {
	mu        sync.Mutex
	lookups   int
	exchanges int
	minted    int
	// exchangeStatus answers the exchange with a refusal when it is set.
	exchangeStatus int
	lookupStatus   int
	lifetime       time.Duration
	issuerAuth     []string
	dataAuth       map[string][]string
}

func newFakeIssuer() *fakeIssuer {
	return &fakeIssuer{lifetime: time.Hour, dataAuth: map[string][]string{}}
}

func (f *fakeIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(r.URL.Path, "/owners/"):
		f.lookups++
		f.issuerAuth = append(f.issuerAuth, r.Header.Get("Authorization"))
		if f.lookupStatus != 0 {
			w.WriteHeader(f.lookupStatus)
			w.Write([]byte(`{"message":"no grant"}`))
			return
		}
		owner := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/owners/"), "/grant")
		body, _ := marshalJSON(map[string]any{"id": "g" + owner})
		w.Write(body)
	case strings.HasPrefix(r.URL.Path, "/grants/") && r.Method == http.MethodPost:
		f.exchanges++
		f.issuerAuth = append(f.issuerAuth, r.Header.Get("Authorization"))
		if f.exchangeStatus != 0 {
			w.WriteHeader(f.exchangeStatus)
			w.Write([]byte(`{"message":"refused"}`))
			return
		}
		f.minted++
		grant := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/grants/"), "/tokens")
		body, _ := marshalJSON(map[string]any{
			"token":      "tok-" + grant + "-" + itoa(f.minted),
			"expires_at": time.Now().Add(f.lifetime).UTC().Format(time.RFC3339),
		})
		w.Write(body)
	case strings.HasPrefix(r.URL.Path, "/widgets/"):
		id := strings.TrimPrefix(r.URL.Path, "/widgets/")
		f.dataAuth[id] = append(f.dataAuth[id], r.Header.Get("Authorization"))
		body, _ := marshalJSON(map[string]any{"id": id, "title": "w" + id})
		w.Write(body)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeIssuer) counts() (lookups, exchanges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.exchanges
}

func (f *fakeIssuer) auth(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dataAuth[id]...)
}

// withAppCredential declares a credential picked per widget id, the way the
// GitHub sample picks an installation.
func withAppCredential(s *Spec) {
	s.Upstream.Forward = []string{"Authorization"}
	s.Upstream.Headers = append(s.Upstream.Headers,
		Header{Name: "Authorization", Value: "token {{ .credential }}", Credential: "app"})
	s.Credentials = append(s.Credentials, &Credential{
		Name:    "app",
		For:     "{{ .key.id }}",
		Headers: []Header{{Name: "Authorization", Value: "Bearer app-jwt"}},
		Lookup:  &CredentialLookup{Method: "GET", Path: "/owners/{{ .for }}/grant", Field: "id"},
		Exchange: CredentialExchange{Method: "POST", Path: "/grants/{{ .lookup }}/tokens",
			Token: "token", Expires: "expires_at"},
	})
}

// sweep refreshes one widget the way the background sweep does: no caller.
// It ages the row first, so the refresh always reaches the upstream.
func sweep(t *testing.T, e *Engine, id string) error {
	t.Helper()
	expire(t, e, "widget", id)
	return e.refresh.refreshOne(context.Background(), "widget", StaleKey{Key: id})
}

func TestCredentialIsExchangedOnceAndReusedUntilNearExpiry(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, withAppCredential, func(s *Spec) {
		s.Refresh = &Refresh{Interval: time.Hour}
	})

	require.Equal(t, http.StatusOK, get(t, e, "/widgets/1").Code)
	expire(t, e, "widget", "1")
	e.refresh.cycle()
	require.Zero(t, e.refresh.Stats().Errors)
	require.NoError(t, sweep(t, e, "1"))

	lookups, exchanges := up.counts()
	assert.Equal(t, 1, lookups, "the lookup answer is reused")
	assert.Equal(t, 1, exchanges, "one token serves every call until it nears expiry")
	assert.Equal(t, []string{"", "token tok-g1-1", "token tok-g1-1"}, up.auth("1"),
		"the caller's own fetch carried nothing; both sweeps carried the token")
	for _, a := range up.issuerAuth {
		assert.Equal(t, "Bearer app-jwt", a, "the lookup and the exchange carry the credential's own header")
	}
}

func TestCredentialIsExchangedAgainInsideItsMargin(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, withAppCredential)
	start := time.Now()
	creds := e.up.creds

	require.NoError(t, sweep(t, e, "1"))
	creds.now = func() time.Time { return start.Add(54 * time.Minute) }
	require.NoError(t, sweep(t, e, "1"))
	_, exchanges := up.counts()
	require.Equal(t, 1, exchanges, "outside the five-minute margin the token is still live")

	creds.now = func() time.Time { return start.Add(56 * time.Minute) }
	require.NoError(t, sweep(t, e, "1"))
	lookups, exchanges := up.counts()
	assert.Equal(t, 2, exchanges, "inside the margin the engine mints a new token")
	assert.Equal(t, 1, lookups, "the lookup has its own ttl")
	assert.Equal(t, []string{"token tok-g1-1", "token tok-g1-1", "token tok-g1-2"}, up.auth("1"))
}

func TestCredentialDiffersPerOwner(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, withAppCredential)

	require.NoError(t, sweep(t, e, "1"))
	require.NoError(t, sweep(t, e, "2"))
	require.NoError(t, sweep(t, e, "1"))

	lookups, exchanges := up.counts()
	assert.Equal(t, 2, lookups)
	assert.Equal(t, 2, exchanges)
	assert.Equal(t, []string{"token tok-g1-1", "token tok-g1-1"}, up.auth("1"))
	assert.Equal(t, []string{"token tok-g2-2"}, up.auth("2"), "each owner gets the token of its own grant")
}

func TestFailedExchangeFailsTheBackgroundRequest(t *testing.T) {
	up := newFakeIssuer()
	up.exchangeStatus = http.StatusInternalServerError
	e, _ := newTestEngine(t, up, withAppCredential)

	err := sweep(t, e, "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential app: exchange POST /grants/g1/tokens answered 500")
	assert.Empty(t, up.auth("1"), "the data request never left without its credential")

	require.Error(t, sweep(t, e, "1"))
	_, exchanges := up.counts()
	assert.Equal(t, 2, exchanges, "a transient failure is not remembered")
}

func TestRefusedExchangeIsRememberedAndStillFails(t *testing.T) {
	up := newFakeIssuer()
	up.exchangeStatus = http.StatusUnauthorized
	e, _ := newTestEngine(t, up, withAppCredential)

	for range 3 {
		err := sweep(t, e, "1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "answered 401")
	}
	_, exchanges := up.counts()
	assert.Equal(t, 1, exchanges, "an authoritative refusal is not asked again inside a minute")
	assert.Empty(t, up.auth("1"))
}

func TestFailedLookupNamesItself(t *testing.T) {
	up := newFakeIssuer()
	up.lookupStatus = http.StatusNotFound
	e, _ := newTestEngine(t, up, withAppCredential)

	err := sweep(t, e, "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential app: lookup GET /owners/1/grant answered 404")
	assert.Empty(t, up.auth("1"))
}

func TestCallerNeverCarriesTheBackgroundCredential(t *testing.T) {
	for name, forward := range map[string][]string{"forwarded": {"Authorization"}, "not forwarded": nil} {
		t.Run(name, func(t *testing.T) {
			up := newFakeIssuer()
			e, _ := newTestEngine(t, up, withAppCredential, func(s *Spec) { s.Upstream.Forward = forward })

			require.Equal(t, http.StatusOK, get(t, e, "/widgets/1").Code)
			req := httptest.NewRequest(http.MethodGet, "/widgets/2", nil)
			req.Header.Set("Authorization", "token caller")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)

			assert.Equal(t, []string{""}, up.auth("1"), "an anonymous caller reaches the upstream anonymous")
			if forward != nil {
				assert.Equal(t, []string{"token caller"}, up.auth("2"))
			} else {
				assert.Equal(t, []string{""}, up.auth("2"))
			}
			lookups, exchanges := up.counts()
			assert.Zero(t, lookups+exchanges, "a caller's request never starts an exchange")
		})
	}
}

func TestCredentialOffFallsBackToTheStaticHeaderBesideIt(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, func(s *Spec) {
		s.Upstream.Headers = append(s.Upstream.Headers, Header{Name: "Authorization", Value: "token static"})
	}, withAppCredential, func(s *Spec) {
		s.Credentials[0].Requires = "env.MIRROR_TEST_UNSET_APP_ID"
	})

	require.NoError(t, sweep(t, e, "1"))
	assert.Equal(t, []string{"token static"}, up.auth("1"))
	lookups, exchanges := up.counts()
	assert.Zero(t, lookups+exchanges)
}

func TestCredentialWithAnEmptyForFails(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, withAppCredential, func(s *Spec) {
		s.Credentials[0].For = "{{ .key.owner }}"
	})
	err := sweep(t, e, "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "for= is empty for this call")
	assert.Empty(t, up.auth("1"))
}

func TestCredentialWithoutLookupUsesOneToken(t *testing.T) {
	up := newFakeIssuer()
	e, _ := newTestEngine(t, up, withAppCredential, func(s *Spec) {
		c := s.Credentials[0]
		c.For, c.Lookup = "", nil
		c.Exchange.Path = "/grants/all/tokens"
	})
	require.NoError(t, sweep(t, e, "1"))
	require.NoError(t, sweep(t, e, "2"))
	_, exchanges := up.counts()
	assert.Equal(t, 1, exchanges)
	assert.Equal(t, []string{"token tok-all-1"}, up.auth("2"))
}
