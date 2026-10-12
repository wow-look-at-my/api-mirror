package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity says how a caller's credential becomes a stable principal. A
// principal gates what the reveal layer shows; it never partitions storage.
//
// The type itself is declared in config.go, where both branches' fields
// meet: the rule-based shape (TTL, User, Assertion) the identities resolver
// below reads, and the flat assertion shape (Header, Method, Path, Send,
// Scheme, ID, Name, Prefix) the Identifier resolver further down reads.

// IdentityRule is a single upstream question whose answer names a principal.
type IdentityRule struct {
	// Header carries an assertion on the inbound request.
	Header string
	// As is the forwarded header the assertion is sent upstream in.
	As string
	// Scheme prefixes the assertion, as in "Bearer".
	Scheme string
	// Path is asked with GET. A 2xx answer is the proof.
	Path string
	// Principal is template source over the answer, `.doc`.
	Principal string
	// Name is template source over the same answer naming the principal for
	// a human. Display only, never a key.
	Name string
}

// validate refuses an identity block that would resolve nobody, or would ask
// the upstream without the credential it is asking about.
func (id *Identity) validate(forward []string) error {
	if id == nil {
		return nil
	}
	if id.TTL <= 0 {
		return fmt.Errorf("<identity> needs a positive ttl: an unremembered verdict costs an upstream call on every request")
	}
	if id.User == nil && id.Assertion == nil {
		return fmt.Errorf("<identity> declares neither <user> nor <assertion>")
	}
	forwarded := func(name string) bool {
		for _, f := range forward {
			if http.CanonicalHeaderKey(f) == http.CanonicalHeaderKey(name) {
				return true
			}
		}
		return false
	}
	if u := id.User; u != nil {
		if u.Path == "" || u.Principal == "" {
			return fmt.Errorf("<identity><user> needs a path and a principal")
		}
		if !forwarded("Authorization") {
			return fmt.Errorf("<identity><user> asks with the caller's Authorization, which <upstream> does not forward")
		}
	}
	if a := id.Assertion; a != nil {
		if a.Header == "" || a.As == "" || a.Path == "" || a.Principal == "" {
			return fmt.Errorf("<identity><assertion> needs a header, an as, a path and a principal")
		}
		if !forwarded(a.As) {
			return fmt.Errorf("<identity><assertion> sends the assertion as %s, which <upstream> does not forward", a.As)
		}
	}
	return nil
}

// maxIdentities bounds the verdict cache; past it, expired verdicts are swept.
const maxIdentities = 10000

// identities resolves and remembers who a credential belongs to.
type identities struct {
	spec *Identity
	up   *Upstreamer
	vars map[string]any
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]identityVerdict
}

type identityVerdict struct {
	principal string
	name      string
	expires   time.Time
}

// identityRefusal is a request the mirror answers itself, without a principal.
type identityRefusal struct {
	status  int
	message string
}

func newIdentities(spec *Identity, up *Upstreamer, vars map[string]any) *identities {
	return &identities{spec: spec, up: up, vars: vars, now: time.Now, cache: map[string]identityVerdict{}}
}

// resolve names who is asking. No credential is no principal. With no
// <identity> declared, the credential's fingerprint is the principal.
func (id *identities) resolve(ctx context.Context, r *http.Request) (string, *identityRefusal) {
	auth := r.Header.Get("Authorization")
	if id == nil || id.spec == nil {
		if auth == "" {
			return "", nil
		}
		return "token:" + fingerprint(auth), nil
	}
	if a := id.spec.Assertion; a != nil {
		if claim := r.Header.Get(a.Header); claim != "" {
			return id.assert(ctx, a, claim)
		}
	}
	if bearerAsserts(r) {
		return id.assert(ctx, id.spec.Assertion, bearerOf(auth))
	}
	if auth == "" {
		return "", nil
	}
	if id.spec.User == nil {
		return "token:" + fingerprint(auth), nil
	}
	return id.user(ctx, id.spec.User, auth, r.Header)
}

// assert verifies an asserted identity with the upstream. Anything short of a
// 2xx is a refusal: a principal the upstream did not vouch for reveals another
// caller's grants.
func (id *identities) assert(ctx context.Context, rule *IdentityRule, claim string) (string, *identityRefusal) {
	cacheKey := "assert:" + fingerprint(claim)
	if v, ok := id.cached(cacheKey); ok {
		return v.principal, nil
	}
	value := claim
	if rule.Scheme != "" {
		value = rule.Scheme + " " + claim
	}
	send := http.Header{}
	send.Set(rule.As, value)
	answer, err := id.up.Call(withLane(ctx, LaneIdentity, "", "assertion"), http.MethodGet, rule.Path, id.vars, send, nil)
	if err != nil || id.up.Transient(answer) || id.up.RateLimited(answer) {
		return "", &identityRefusal{http.StatusServiceUnavailable, "could not verify the identity assertion; retry"}
	}
	if answer.Status < 200 || answer.Status >= 300 {
		return "", &identityRefusal{http.StatusUnauthorized, "could not verify the identity assertion"}
	}
	v, err := id.verdict(rule, answer)
	if err != nil {
		logf("identity assertion: %v", err)
		return "", &identityRefusal{http.StatusUnauthorized, "could not verify the identity assertion"}
	}
	id.remember(cacheKey, v)
	return v.principal, nil
}

// user resolves a bearer through the upstream's "who am I". A credential the
// upstream says is not a user keeps its fingerprint as its principal; a single
// it rejects is refused; a single it cannot answer about right now fails this
// request, because guessing a principal reveals another caller's grants.
func (id *identities) user(ctx context.Context, rule *IdentityRule, auth string, h http.Header) (string, *identityRefusal) {
	fp := fingerprint(auth)
	cacheKey := "user:" + fp
	if v, ok := id.cached(cacheKey); ok {
		return v.principal, nil
	}
	answer, err := id.up.Call(withLane(ctx, LaneIdentity, "", "user"), http.MethodGet, rule.Path, id.vars, h, nil)
	switch {
	case err != nil || id.up.Transient(answer) || id.up.RateLimited(answer):
		return "", &identityRefusal{http.StatusServiceUnavailable, "could not resolve the credential's identity; retry"}
	case answer.Status == http.StatusUnauthorized:
		return "", &identityRefusal{http.StatusUnauthorized, "the upstream rejected the credential"}
	case answer.Status == http.StatusNotFound || answer.Status == http.StatusForbidden:
		v := identityVerdict{principal: "token:" + fp, expires: id.now().Add(id.spec.TTL)}
		id.remember(cacheKey, v)
		return v.principal, nil
	case answer.Status < 200 || answer.Status >= 300:
		return "", &identityRefusal{http.StatusServiceUnavailable, fmt.Sprintf("resolving the credential's identity answered %d; retry", answer.Status)}
	}
	v, err := id.verdict(rule, answer)
	if err != nil {
		logf("identity user: %v", err)
		return "", &identityRefusal{http.StatusServiceUnavailable, "could not resolve the credential's identity; retry"}
	}
	id.remember(cacheKey, v)
	return v.principal, nil
}

// verdict renders the principal a 2xx answer names. An empty a single is an
// error: every credential would share it.
func (id *identities) verdict(rule *IdentityRule, answer *Answer) (identityVerdict, error) {
	doc, err := decodeJSON(answer.Body)
	if err != nil {
		return identityVerdict{}, fmt.Errorf("%s: %w", rule.Path, err)
	}
	data := map[string]any{"doc": doc}
	for k, v := range id.vars {
		data[k] = v
	}
	principal, err := renderString(rule.Principal, data)
	if err != nil {
		return identityVerdict{}, fmt.Errorf("%s principal: %w", rule.Path, err)
	}
	if principal == "" {
		return identityVerdict{}, fmt.Errorf("%s: the principal rendered empty", rule.Path)
	}
	v := identityVerdict{principal: principal, expires: id.now().Add(id.spec.TTL)}
	if rule.Name != "" {
		// A name that does not render leaves the principal unnamed: it is
		// display text, and the verdict it rides on is already proven.
		if name, err := renderString(rule.Name, data); err == nil {
			v.name = strings.TrimSpace(name)
		} else {
			logf("%s name: %v", rule.Path, err)
		}
	}
	return v, nil
}

// names maps each principal a live verdict resolved to the name its upstream
// answer gave it.
func (id *identities) names() map[string]string {
	out := map[string]string{}
	if id == nil {
		return out
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	now := id.now()
	for _, v := range id.cache {
		if v.name != "" && now.Before(v.expires) {
			out[v.principal] = v.name
		}
	}
	return out
}

func (id *identities) cached(key string) (identityVerdict, bool) {
	id.mu.Lock()
	defer id.mu.Unlock()
	v, ok := id.cache[key]
	if !ok || !id.now().Before(v.expires) {
		return identityVerdict{}, false
	}
	return v, true
}

func (id *identities) remember(key string, v identityVerdict) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if len(id.cache) >= maxIdentities {
		now := id.now()
		for k, old := range id.cache {
			if !now.Before(old.expires) {
				delete(id.cache, k)
			}
		}
		if len(id.cache) >= maxIdentities {
			logf("identity cache full at %d live verdicts; resolving %s without remembering it", len(id.cache), v.principal)
			return
		}
	}
	id.cache[key] = v
}

// bearerOf strips the scheme from an Authorization value.
func bearerOf(auth string) string {
	if _, token, ok := strings.Cut(strings.TrimSpace(auth), " "); ok {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(auth)
}

// assertingRoute reports whether a route declaring assert="true" answers r.
func (e *Engine) assertingRoute(r *http.Request) bool {
	for _, rt := range e.spec.Routes {
		if !rt.Assert || rt.Method != r.Method {
			continue
		}
		if _, ok := matchPath(rt.Path, r.URL.EscapedPath()); ok {
			return true
		}
	}
	return false
}

type assertKey struct{}

// withBearerAssertion marks a request whose bearer is itself an assertion.
func withBearerAssertion(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), assertKey{}, true))
}

func bearerAsserts(r *http.Request) bool {
	v, _ := r.Context().Value(assertKey{}).(bool)
	return v && r.Header.Get("Authorization") != ""
}

// credentialValue fills a credential key: the caller's principal, or their
// credential's fingerprint. Empty means the caller has neither.
func credentialValue(k Key, r *http.Request) string {
	if k.ByPrincipal {
		return principalOf(r)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	return fingerprint(auth)
}

type principalKey struct{}

func withPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// defaultIdentityTTL bounds a verdict when the spec names none.
const defaultIdentityTTL = 10 * time.Minute

// maxIdentityVerdicts bounds the verdict cache.
const maxIdentityVerdicts = 4096

// Identifier resolves the principal of a request.
type Identifier struct {
	spec   *Identity
	whoami *Whoami
	up     *Upstreamer
	vars   map[string]any

	mu       sync.Mutex
	verdicts map[string]identityVerdict
}

// NewIdentifier returns a resolver. A nil spec resolves every caller
// by the fingerprint of its credential.
func NewIdentifier(spec *Identity, up *Upstreamer, vars map[string]any) *Identifier {
	return &Identifier{spec: spec, up: up, vars: vars, verdicts: map[string]identityVerdict{}}
}

// IdentityError is a request whose identity cannot be established.
// Status is what the caller receives.
type IdentityError struct {
	Status int
	Reason string
}

func (e *IdentityError) Error() string { return e.Reason }

// Resolve names who is asking.
//
// An assertion that fails verification is an error. It never falls back
// to the credential fingerprint: a caller that asked to be someone must
// not quietly become someone else.
func (id *Identifier) Resolve(ctx context.Context, r *http.Request) (string, error) {
	assertion := ""
	if id.spec != nil {
		assertion = r.Header.Get(id.spec.Header)
	}
	if assertion == "" {
		return id.owner(ctx, r), nil
	}
	sum := sha256.Sum256([]byte(assertion))
	cacheKey := hex.EncodeToString(sum[:])
	if v, ok := id.cached(cacheKey); ok {
		return v.principal, nil
	}
	v, err := id.verify(ctx, assertion)
	if err != nil {
		return "", err
	}
	id.store(cacheKey, v)
	return v.principal, nil
}

// owner names the caller by the owner of its credential when <whoami> is
// declared, and by the credential's fingerprint otherwise.
//
// A credential the upstream will not name, such as an installation token
// that cannot call the endpoint, keeps its fingerprint. The upstream will
// not also name that answer is cached like any other. A transient failure
// keeps the fingerprint for this request only.
func (id *Identifier) owner(ctx context.Context, r *http.Request) string {
	fallback := tokenPrincipal(r)
	if id.whoami == nil || fallback == "" {
		return fallback
	}
	cacheKey := "whoami:" + fallback
	if v, ok := id.cached(cacheKey); ok {
		return v.principal
	}
	set := http.Header{}
	set.Set("Authorization", r.Header.Get("Authorization"))
	ans, err := id.up.CallWith(withLane(ctx, LaneIdentity, fallback, id.whoami.Prefix),
		id.whoami.Method, id.whoami.Path, id.vars, nil, set, nil)
	if err != nil || id.up.Transient(ans) {
		return fallback
	}
	principal := fallback
	if ans.Status >= 200 && ans.Status < 300 {
		if doc, err := decodeJSON(ans.Body); err == nil {
			if who := scalarString(lookupPath(doc, id.whoami.ID)); who != "" {
				principal = id.whoami.Prefix + ":" + who
			}
		}
	}
	ttl := id.whoami.TTL
	if ttl <= 0 {
		ttl = defaultIdentityTTL
	}
	id.store(cacheKey, identityVerdict{principal: principal, expires: time.Now().Add(ttl)})
	return principal
}

func (id *Identifier) cached(key string) (identityVerdict, bool) {
	id.mu.Lock()
	defer id.mu.Unlock()
	v, ok := id.verdicts[key]
	if !ok {
		return identityVerdict{}, false
	}
	if time.Now().After(v.expires) {
		delete(id.verdicts, key)
		return identityVerdict{}, false
	}
	return v, true
}

// store keeps a verdict. A full cache sheds its expired entries first.
// If that is not enough, it drops the whole cache and logs the count.
// The cost of the drop is one extra verification per live caller.
func (id *Identifier) store(key string, v identityVerdict) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if len(id.verdicts) >= maxIdentityVerdicts {
		now := time.Now()
		for k, old := range id.verdicts {
			if now.After(old.expires) {
				delete(id.verdicts, k)
			}
		}
		if len(id.verdicts) >= maxIdentityVerdicts {
			logf("identity: verdict cache full, dropped %d live verdicts", len(id.verdicts))
			clear(id.verdicts)
		}
	}
	id.verdicts[key] = v
}

// verify asks the upstream whether the assertion is genuine. Only a
// success is kept. A refusal is not cached, because a caller that
// fixes its assertion must not wait out a stored refusal.
func (id *Identifier) verify(ctx context.Context, assertion string) (identityVerdict, error) {
	value := assertion
	if id.spec.Scheme != "" {
		value = id.spec.Scheme + " " + assertion
	}
	set := http.Header{}
	set.Set(id.spec.Send, value)
	ctx = withLane(ctx, LaneIdentity, "", id.spec.Prefix)
	ans, err := id.up.CallWith(ctx, id.spec.Method, id.spec.Path, id.vars, nil, set, nil)
	if err != nil {
		return identityVerdict{}, &IdentityError{Status: http.StatusBadGateway, Reason: "verify identity: " + err.Error()}
	}
	if id.up.Transient(ans) {
		return identityVerdict{}, &IdentityError{Status: http.StatusBadGateway,
			Reason: fmt.Sprintf("verify identity: upstream answered %d, try again", ans.Status)}
	}
	if ans.Status < 200 || ans.Status >= 300 {
		return identityVerdict{}, &IdentityError{Status: http.StatusUnauthorized,
			Reason: fmt.Sprintf("identity assertion refused by the upstream (%d)", ans.Status)}
	}
	doc, err := decodeJSON(ans.Body)
	if err != nil {
		return identityVerdict{}, &IdentityError{Status: http.StatusBadGateway, Reason: "verify identity: " + err.Error()}
	}
	who := scalarString(lookupPath(doc, id.spec.ID))
	if who == "" {
		return identityVerdict{}, &IdentityError{Status: http.StatusUnauthorized,
			Reason: fmt.Sprintf("identity answer carries no %s", id.spec.ID)}
	}
	v := identityVerdict{principal: id.spec.Prefix + ":" + who, expires: time.Now().Add(id.ttl())}
	if id.spec.Name != "" {
		v.name = scalarString(lookupPath(doc, id.spec.Name))
	}
	return v, nil
}

func (id *Identifier) ttl() time.Duration {
	if id.spec.TTL > 0 {
		return id.spec.TTL
	}
	return defaultIdentityTTL
}

// scalarString renders a JSON scalar. An object, an array or an absent
// value renders empty, because none of them can name a principal.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil, map[string]any, []any:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// tokenPrincipal names a caller by the full SHA-256 of its bare credential.
func tokenPrincipal(r *http.Request) string {
	cred := bareCredential(r.Header.Get("Authorization"))
	if cred == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cred))
	return "token:" + hex.EncodeToString(sum[:])
}

// bareCredential strips a Bearer or token scheme word from an Authorization
// value. Any other scheme stays part of the value.
func bareCredential(auth string) string {
	for _, scheme := range []string{"Bearer ", "token "} {
		if len(auth) >= len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
			return strings.TrimSpace(auth[len(scheme):])
		}
	}
	return strings.TrimSpace(auth)
}

func (id *Identity) validate(forward []string) error {
	if id == nil {
		return nil
	}
	forwarded := func(name string) bool {
		for _, f := range forward {
			if http.CanonicalHeaderKey(f) == http.CanonicalHeaderKey(name) {
				return true
			}
		}
		return false
	}
	if u := id.User; u != nil {
		if u.Path == "" || u.Principal == "" {
			return fmt.Errorf("<identity><user> needs a path and a principal")
		}
		if !forwarded("Authorization") {
			return fmt.Errorf("<identity><user> asks with the caller's Authorization, which <upstream> does not forward")
		}
	}
	if a := id.Assertion; a != nil {
		if a.Header == "" || a.As == "" || a.Path == "" || a.Principal == "" {
			return fmt.Errorf("<identity><assertion> needs a header, an as, a path and a principal")
		}
		if !forwarded(a.As) {
			return fmt.Errorf("<identity><assertion> sends the assertion as %s, which <upstream> does not forward", a.As)
		}
	}
	if id.Path != "" {
		for attr, v := range map[string]string{
			"header": id.Header, "path": id.Path, "id": id.ID, "prefix": id.Prefix,
		} {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("<identity> needs %s=", attr)
			}
		}
		if !strings.HasPrefix(id.Path, "/") {
			return fmt.Errorf("<identity> path %q must start with /", id.Path)
		}
		if strings.Contains(id.Prefix, ":") {
			return fmt.Errorf("<identity> prefix %q must not contain ':', the separator of a principal", id.Prefix)
		}
		for _, name := range forward {
			// The assertion names the caller to the mirror. Sent upstream, it
			// would reach an API that never asked for it.
			if strings.EqualFold(name, id.Header) {
				return fmt.Errorf("<identity> header %s is also a <forward>, so the assertion would leave for the upstream", id.Header)
			}
		}
		if id.TTL < 0 {
			return fmt.Errorf("<identity> ttl must not be negative")
		}
	}
	return nil
}
