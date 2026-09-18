package mirror

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity says how a caller's credential becomes a stable principal. A
// principal gates what the reveal layer shows; it never partitions storage.
type Identity struct {
	// TTL bounds how long a resolved credential is trusted without asking again.
	TTL time.Duration
	// User resolves a bearer through the upstream's own "who am I" answer.
	User *IdentityRule
	// Assertion lets a caller name an identity the upstream verifies, so
	// credentials that rotate keep the grants their holder earned.
	Assertion *IdentityRule
}

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
	return identityVerdict{principal: principal, expires: id.now().Add(id.spec.TTL)}, nil
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
