package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// defaultCredentialMargin replaces a token this long before it expires.
const defaultCredentialMargin = 5 * time.Minute

// defaultLookupTTL bounds a lookup answer when the spec names no ttl.
const defaultLookupTTL = time.Hour

// credentialRefusalTTL keeps an authoritative refusal, so a missing grant costs one call a minute rather than one call.
const credentialRefusalTTL = time.Minute

// maxCredentialEntries bounds the cache of lookups and tokens.
const maxCredentialEntries = 4096

// credentials obtains and caches the tokens of the declared <credential>s.
type credentials struct {
	up     *Upstreamer
	vars   map[string]any
	byName map[string]*Credential
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*credentialEntry
}

// credentialEntry is one cached lookup answer or token. Its own mutex
// makes concurrent callers for one key share a single exchange.
type credentialEntry struct {
	mu    sync.Mutex
	value string
	err   error
	until time.Time
}

func newCredentials(up *Upstreamer, spec *Spec, vars map[string]any) *credentials {
	c := &credentials{up: up, vars: vars, byName: map[string]*Credential{}, now: time.Now, entries: map[string]*credentialEntry{}}
	for _, cred := range spec.Credentials {
		c.byName[cred.Name] = cred
	}
	return c
}

// exchangeKey marks a context as inside an exchange.
type exchangeKey struct{}

func inExchange(ctx context.Context) bool {
	v, _ := ctx.Value(exchangeKey{}).(bool)
	return v
}

// missing names the empty requirement that turns a credential off.
func (c *credentials) missing(cred *Credential) string {
	if cred.Requires == "" {
		return ""
	}
	if v := lookupPath(c.vars, cred.Requires); v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
		return ""
	}
	return cred.Requires
}

// announce logs which credentials are off, and what a call with no caller
// sends instead.
func (c *credentials) announce() {
	for _, cred := range c.byName {
		if want := c.missing(cred); want != "" {
			logf("credential %s: OFF because %s is empty. A call with no caller sends the other upstream headers.", cred.Name, want)
		} else {
			logger.Info(fmt.Sprintf("credential %s: on", cred.Name))
		}
	}
}

// token returns the named credential's token for a call about key. It
// reports false when the credential is off. A failed lookup or exchange
// is an error, never an anonymous call.
func (c *credentials) token(ctx context.Context, name string, key map[string]string) (string, bool, error) {
	cred := c.byName[name]
	if cred == nil {
		return "", false, fmt.Errorf("credential %s: not declared", name)
	}
	if c.missing(cred) != "" {
		return "", false, nil
	}
	scope := map[string]any{"env": c.vars["env"], "var": c.vars["var"], "key": key}
	forValue := ""
	if cred.For != "" {
		v, err := renderString(cred.For, scope)
		if err != nil {
			return "", false, fmt.Errorf("credential %s: for=: %w", name, err)
		}
		if forValue = strings.TrimSpace(v); forValue == "" {
			return "", false, fmt.Errorf("credential %s: for= is empty for this call, so no token covers it", name)
		}
	}
	scope["for"] = forValue
	ctx = context.WithValue(ctx, exchangeKey{}, true)

	if l := cred.Lookup; l != nil {
		path, err := renderString(l.Path, scope)
		if err != nil {
			return "", false, fmt.Errorf("credential %s: lookup path: %w", name, err)
		}
		found, err := c.cached("lookup\x00"+name+"\x00"+forValue, func() (string, time.Time, error) {
			doc, err := c.call(ctx, cred, "lookup", l.Method, path, scope)
			if err != nil {
				return "", time.Time{}, err
			}
			v := stringAt(doc, l.Field)
			if v == "" {
				return "", time.Time{}, fmt.Errorf("credential %s: lookup %s %s answered with nothing at %s", name, l.Method, path, l.Field)
			}
			return v, c.now().Add(cmpDuration(l.TTL, defaultLookupTTL)), nil
		})
		if err != nil {
			return "", false, err
		}
		scope["lookup"] = found
	}

	x := cred.Exchange
	path, err := renderString(x.Path, scope)
	if err != nil {
		return "", false, fmt.Errorf("credential %s: exchange path: %w", name, err)
	}
	tok, err := c.cached("exchange\x00"+name+"\x00"+x.Method+" "+path, func() (string, time.Time, error) {
		doc, err := c.call(ctx, cred, "exchange", x.Method, path, scope)
		if err != nil {
			return "", time.Time{}, err
		}
		tok := stringAt(doc, x.Token)
		if tok == "" {
			return "", time.Time{}, fmt.Errorf("credential %s: exchange %s %s answered with no token at %s", name, x.Method, path, x.Token)
		}
		expires := c.now().Add(x.TTL)
		if x.Expires != "" {
			at, ok := timeAt(doc, x.Expires)
			if !ok {
				return "", time.Time{}, fmt.Errorf("credential %s: exchange %s %s states no expiry at %s", name, x.Method, path, x.Expires)
			}
			expires = at
		}
		return tok, expires.Add(-cmpDuration(cred.Margin, defaultCredentialMargin)), nil
	})
	if err != nil {
		return "", false, err
	}
	return tok, true, nil
}

func cmpDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// call makes a lookup or an exchange request with the credential's own
// headers, and decodes a 2xx answer.
func (c *credentials) call(ctx context.Context, cred *Credential, step, method, path string, scope map[string]any) (any, error) {
	set := http.Header{}
	for _, h := range cred.Headers {
		v, err := renderString(h.Value, scope)
		if err != nil {
			return nil, fmt.Errorf("credential %s: header %s: %w", cred.Name, h.Name, err)
		}
		if v != "" {
			set.Set(h.Name, v)
		}
	}
	ctx = withLane(ctx, LaneCredential, "", cred.Name+" "+step)
	ans, err := c.up.CallWith(ctx, method, path, c.vars, nil, set, nil)
	if err != nil {
		return nil, &exchangeError{Credential: cred.Name, Step: step, Method: method, Path: path, Err: err}
	}
	if ans.Status < 200 || ans.Status >= 300 {
		return nil, &exchangeError{Credential: cred.Name, Step: step, Method: method, Path: path,
			Status: ans.Status, Authoritative: !c.up.Transient(ans)}
	}
	doc, err := decodeJSON(ans.Body)
	if err != nil {
		return nil, &exchangeError{Credential: cred.Name, Step: step, Method: method, Path: path, Err: err}
	}
	return doc, nil
}

// exchangeError names the credential, the step and the upstream's answer.
type exchangeError struct {
	Credential, Step, Method, Path string
	Status                         int
	// Authoritative is a refusal that asking again will not change.
	Authoritative bool
	Err           error
}

func (e *exchangeError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("credential %s: %s %s %s: %v", e.Credential, e.Step, e.Method, e.Path, e.Err)
	}
	return fmt.Sprintf("credential %s: %s %s %s answered %d", e.Credential, e.Step, e.Method, e.Path, e.Status)
}

func (e *exchangeError) Unwrap() error { return e.Err }

// cached returns a live value for key, or runs fetch. One fetch runs per
// key at a time. An authoritative refusal is kept for a minute; any other
// failure is not kept.
func (c *credentials) cached(key string, fetch func() (string, time.Time, error)) (string, error) {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := c.now()
	if now.Before(e.until) {
		if e.err != nil {
			return "", e.err
		}
		return e.value, nil
	}
	v, until, err := fetch()
	if err != nil {
		var refused *exchangeError
		if errors.As(err, &refused) && refused.Authoritative {
			e.value, e.err, e.until = "", err, now.Add(credentialRefusalTTL)
		} else {
			e.value, e.err, e.until = "", nil, time.Time{}
		}
		return "", err
	}
	// A token that is already inside its margin serves this call only.
	e.value, e.err, e.until = v, nil, until
	return v, nil
}

// entry finds or adds the cache entry for key. A full cache sheds its
// expired entries first. If that is not enough, it drops them all and
// logs the count. The cost is one more exchange per live key.
func (c *credentials) entry(key string) *credentialEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e
	}
	if len(c.entries) >= maxCredentialEntries {
		now := c.now()
		for k, old := range c.entries {
			if old.mu.TryLock() {
				if !now.Before(old.until) {
					delete(c.entries, k)
				}
				old.mu.Unlock()
			}
		}
		if len(c.entries) >= maxCredentialEntries {
			logf("credential: cache full, dropped %d live entries", len(c.entries))
			clear(c.entries)
		}
	}
	e := &credentialEntry{}
	c.entries[key] = e
	return e
}
