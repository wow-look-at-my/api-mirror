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

type identityVerdict struct {
	principal string
	name      string
	expires   time.Time
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
// that cannot call the endpoint, keeps its fingerprint, and that answer is
// cached like any other. A transient failure keeps the fingerprint for
// this request only.
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
	return nil
}
