package mirror

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App is the mirror signing for itself: a JWT from a private key, and the
// per-owner tokens that JWT mints. The mirror's own calls (refresh, check,
// replay) carry these instead of a static background token, because an App's
// reach is per installation and its tokens expire within the hour.
type App struct {
	// ID and Key are template source; KeyFile names a PEM file and wins.
	ID, Key, KeyFile string
	// Installations lists every installation, asked with the JWT.
	Installations string
	// Account is the dotted path in a listed installation naming its owner.
	Account string
	// Mint asks for an installation's token; {id} is the installation.
	Mint string
	// OwnerKey is the resource key whose value names the owner a call is about.
	OwnerKey string
}

const (
	appJWTLife = 9 * time.Minute
	// appClockSkew backdates the JWT for an upstream clock running behind.
	appClockSkew = 60 * time.Second
	// appTokenMargin retires a token before it can expire mid-request.
	appTokenMargin = 5 * time.Minute
	// appListLife bounds how stale the owner-to-installation map may be.
	appListLife = 10 * time.Minute
)

// validate refuses an App that could sign but never mint for an owner.
func (a *App) validate() error {
	if a == nil {
		return nil
	}
	if a.Installations == "" || a.Account == "" || a.Mint == "" || a.OwnerKey == "" {
		return errors.New("<app> needs installations, account, mint and owner-key")
	}
	if !strings.Contains(a.Mint, "{id}") {
		return fmt.Errorf("<app> mint %q names no {id}, so every owner would get one installation's token", a.Mint)
	}
	return nil
}

// appAuth is an App with its key parsed and its tokens remembered.
type appAuth struct {
	rule *App
	up   *Upstreamer
	vars map[string]any
	id   string
	key  *rsa.PrivateKey
	now  func() time.Time

	mu       sync.Mutex
	installs map[string]string
	listed   time.Time
	tokens   map[string]appToken
}

type appToken struct {
	value      string
	serveUntil time.Time
}

// newAppAuth resolves the App's id and key. An App the environment does not
// configure is no App: the mirror's own calls fall back to the declared
// background headers. A key that is present and unreadable is an error.
func newAppAuth(rule *App, vars map[string]any, up *Upstreamer) (*appAuth, error) {
	if rule == nil {
		return nil, nil
	}
	id, err := renderString(rule.ID, vars)
	if err != nil {
		return nil, fmt.Errorf("<app> id: %w", err)
	}
	pemText, err := appKeyText(rule, vars)
	if err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(pemText) == "" {
		logf("<app>: no id or key configured; the mirror's own calls use the background headers")
		return nil, nil
	}
	key, err := parseRSAKey(pemText)
	if err != nil {
		return nil, fmt.Errorf("<app> key: %w", err)
	}
	return &appAuth{rule: rule, up: up, vars: vars, id: id, key: key, now: time.Now,
		installs: map[string]string{}, tokens: map[string]appToken{}}, nil
}

func appKeyText(rule *App, vars map[string]any) (string, error) {
	path, err := renderString(rule.KeyFile, vars)
	if err != nil {
		return "", fmt.Errorf("<app> key-file: %w", err)
	}
	if path = strings.TrimSpace(path); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("<app> key-file: %w", err)
		}
		return string(b), nil
	}
	text, err := renderString(rule.Key, vars)
	if err != nil {
		return "", fmt.Errorf("<app> key: %w", err)
	}
	// An environment variable cannot hold a newline in every deployer, so
	// an escaped a single is a newline.
	return strings.ReplaceAll(text, `\n`, "\n"), nil
}

func parseRSAKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("neither PKCS#1 nor PKCS#8: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return k, nil
}

// jwt signs a fresh assertion of the App's identity.
func (a *appAuth) jwt() (string, error) {
	now := a.now()
	header, err := marshalJSON(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := marshalJSON(map[string]any{
		"iat": now.Add(-appClockSkew).Unix(),
		"exp": now.Add(appJWTLife).Unix(),
		"iss": a.id,
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

type appCallKey struct{}

// withAppCall marks a call the App makes as itself rather than for an owner.
func withAppCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, appCallKey{}, true)
}

// authorization is the credential the mirror's own call carries: the JWT for
// an App-level call, an owner's installation token for a call about that
// owner, and nothing when the call names no owner.
func (a *appAuth) authorization(ctx context.Context) (string, bool, error) {
	if v, _ := ctx.Value(appCallKey{}).(bool); v {
		jwt, err := a.jwt()
		return "Bearer " + jwt, err == nil, err
	}
	plan := planFrom(ctx)
	if plan == nil {
		return "", false, nil
	}
	owner := strings.ToLower(plan.key[a.rule.OwnerKey])
	if owner == "" {
		return "", false, nil
	}
	token, err := a.tokenFor(ctx, owner)
	if err != nil || token == "" {
		return "", false, err
	}
	return "Bearer " + token, true, nil
}

// tokenFor returns a live installation token for an owner, minting a single
// when the remembered token is near expiry. An owner with no installation
// gets no token, and the call goes out with the background headers.
func (a *appAuth) tokenFor(ctx context.Context, owner string) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[owner]
	a.mu.Unlock()
	if ok && a.now().Before(t.serveUntil) {
		return t.value, nil
	}
	install, err := a.installationOf(ctx, owner)
	if err != nil || install == "" {
		return "", err
	}
	answer, err := a.send(ctx, http.MethodPost, strings.ReplaceAll(a.rule.Mint, "{id}", install))
	if err != nil {
		return "", err
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(answer, &minted); err != nil || minted.Token == "" {
		return "", fmt.Errorf("app: minting a token for %s answered no token", owner)
	}
	a.mu.Lock()
	a.tokens[owner] = appToken{value: minted.Token, serveUntil: minted.ExpiresAt.Add(-appTokenMargin)}
	a.mu.Unlock()
	return minted.Token, nil
}

// owners lists every account the App is installed on, relisting when the map
// is older than appListLife.
func (a *appAuth) owners(ctx context.Context) ([]string, error) {
	if _, err := a.installationOf(ctx, ""); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.installs))
	for owner := range a.installs {
		out = append(out, owner)
	}
	sort.Strings(out)
	return out, nil
}

// installationOf maps an owner to its installation, relisting when the map is
// older than appListLife.
func (a *appAuth) installationOf(ctx context.Context, owner string) (string, error) {
	a.mu.Lock()
	id, ok := a.installs[owner]
	fresh := a.now().Sub(a.listed) < appListLife
	a.mu.Unlock()
	if ok || fresh {
		return id, nil
	}
	found := map[string]string{}
	for page := 1; ; page++ {
		sep := "?"
		if strings.Contains(a.rule.Installations, "?") {
			sep = "&"
		}
		body, err := a.send(ctx, http.MethodGet, a.rule.Installations+sep+"per_page=100&page="+strconv.Itoa(page))
		if err != nil {
			return "", err
		}
		doc, err := decodeJSON(body)
		if err != nil {
			return "", fmt.Errorf("app: installations: %w", err)
		}
		items, _ := doc.([]any)
		for _, item := range items {
			account := strings.ToLower(fmt.Sprint(lookupPath(item, a.rule.Account)))
			if installID := lookupPath(item, "id"); installID != nil && account != "" {
				found[account] = fmt.Sprint(installID)
			}
		}
		if len(items) < 100 {
			break
		}
	}
	a.mu.Lock()
	a.installs = found
	a.listed = a.now()
	a.mu.Unlock()
	return found[owner], nil
}

// send asks the upstream as the App, with its JWT and the static headers.
func (a *appAuth) send(ctx context.Context, method, path string) ([]byte, error) {
	jwt, err := a.jwt()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(withLane(ctx, LaneRefresh, "app:"+a.id, "app-token"), method, a.up.base+path, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range a.up.headers {
		if h.Background {
			continue
		}
		value, err := renderString(h.Value, a.vars)
		if err != nil {
			return nil, fmt.Errorf("app: header %s: %w", h.Name, err)
		}
		if value != "" {
			req.Header.Set(h.Name, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := a.up.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("app %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("app %s %s answered %d", method, path, resp.StatusCode)
	}
	return body, nil
}
