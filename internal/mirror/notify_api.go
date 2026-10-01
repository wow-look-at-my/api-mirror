package mirror

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
)

// The public subscription API, at <notify path=>. A caller manages only its
// own registrations, under the principal the identity layer resolves.

func (no *Notify) validate(resources map[string]*Resource, dashboard string) error {
	if no.Path != "" {
		if !strings.HasPrefix(no.Path, "/") {
			return fmt.Errorf("<notify> path %q must start with /", no.Path)
		}
		// The subscription path is matched ahead of the dashboard, so it may sit
		// under the dashboard prefix. It may not be the dashboard itself.
		if no.Path == dashboard || no.Path == dashboard+"/api" || strings.HasPrefix(no.Path, dashboard+"/api/") {
			return fmt.Errorf("<notify> path %q would hide the dashboard's own routes", no.Path)
		}
	}
	seen := set.New[string]()
	for _, f := range no.Fields {
		if f.Field == "" {
			return fmt.Errorf("<notify><field> needs a name")
		}
		if seen.Contains(f.Field) {
			return fmt.Errorf("<notify><field name=%q> is declared twice", f.Field)
		}
		seen.Add(f.Field)
		if (f.From == "") == (f.Expr == "") {
			return fmt.Errorf("<notify><field name=%q>: give it a payload path or an expr, not both and not neither", f.Field)
		}
	}
	if err := oneOutputPerInput("<notify>", no.Fields); err != nil {
		return err
	}
	if no.Reveal == nil {
		return nil
	}
	res, ok := resources[no.Reveal.Resource]
	if !ok {
		return fmt.Errorf("<notify><reveal resource=%q> names no declared resource", no.Reveal.Resource)
	}
	if res.Reveal == nil || res.Reveal.Credential {
		return fmt.Errorf("<notify><reveal resource=%q> needs a resource with a public or grant rule", no.Reveal.Resource)
	}
	for _, k := range res.Keys {
		if !slices.ContainsFunc(no.Reveal.Keys, func(s Set) bool { return s.Field == k.Name }) {
			return fmt.Errorf("<notify><reveal> names one row, so it needs <key field=%q>", k.Name)
		}
	}
	return nil
}

// handlesSubscriptions reports whether a path is the subscription API.
func (e *Engine) handlesSubscriptions(path string) bool {
	if e.notify == nil || e.spec.Notify.Path == "" {
		return false
	}
	p := e.spec.Notify.Path
	return path == p || strings.HasPrefix(path, p+"/")
}

func (e *Engine) serveSubscriptions(w *recorder, r *http.Request) {
	w.note(DispAdmin, e.spec.Notify.Path, "", "subscriptions")
	principal, err := e.identity.Resolve(r.Context(), r)
	if err != nil {
		status := http.StatusBadGateway
		var ie *IdentityError
		if errors.As(err, &ie) {
			status = ie.Status
		}
		http.Error(w, err.Error(), status)
		return
	}
	if principal == "" {
		// A registration belongs to somebody.
		http.Error(w, "a subscription needs a credential", http.StatusUnauthorized)
		return
	}
	w.principal = principal

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, e.spec.Notify.Path), "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		e.listOwnSubscriptions(w, r, principal)
	case id == "" && r.Method == http.MethodPost:
		e.createOwnSubscription(w, r, principal)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodGet:
		e.getOwnSubscription(w, r, principal, id)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodPatch:
		e.patchOwnSubscription(w, r, principal, id)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodDelete:
		gone, err := e.notify.Subs().Delete(r.Context(), principal, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !gone {
			http.Error(w, "no such subscription", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// scopeField is the JSON name a subscription's scope filter travels under.
func (e *Engine) scopeField() string {
	return cmp.Or(e.spec.Notify.ScopeField, "scopes")
}

// subscriptionView renders a subscription for its owner.
func (e *Engine) subscriptionView(s Subscription) map[string]any {
	disabledReason := ""
	if s.Disabled {
		disabledReason = cmp.Or(s.LastError, "disabled")
	}
	return map[string]any{
		"id":                   s.ID,
		"principal":            s.Principal,
		"url":                  s.URL,
		e.scopeField():         nonNil(s.Scopes),
		"events":               nonNil(s.Events),
		"active":               !s.Disabled,
		"consecutive_failures": s.Failures,
		"disabled_reason":      disabledReason,
		"created_at":           stamp(s.CreatedAt),
		"updated_at":           stamp(s.UpdatedAt),
		"last_success_at":      stamp(s.LastOK),
		"last_failure_at":      stamp(s.LastFailure),
		"last_error":           s.LastError,
	}
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (e *Engine) listOwnSubscriptions(w http.ResponseWriter, r *http.Request, principal string) {
	subs, err := e.notify.Subs().ByPrincipal(r.Context(), principal)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(subs))
	for _, s := range subs {
		out = append(out, e.subscriptionView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) getOwnSubscription(w http.ResponseWriter, r *http.Request, principal, id string) {
	sub, err := e.notify.Subs().Get(r.Context(), principal, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sub == nil {
		http.Error(w, "no such subscription", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, e.subscriptionView(*sub))
}

// readSubscriptionBody decodes a request body into its raw fields, so a
// patch can tell an absent field from an empty one.
func readSubscriptionBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		http.Error(w, "the body is not a JSON object", http.StatusBadRequest)
		return nil, false
	}
	return fields, true
}

// field decodes one named field into dst, and reports whether it was sent.
func field(w http.ResponseWriter, fields map[string]json.RawMessage, name string, dst any) (bool, bool) {
	raw, ok := fields[name]
	if !ok {
		return false, true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		http.Error(w, name+": "+err.Error(), http.StatusBadRequest)
		return true, false
	}
	return true, true
}

func (e *Engine) createOwnSubscription(w http.ResponseWriter, r *http.Request, principal string) {
	fields, ok := readSubscriptionBody(w, r)
	if !ok {
		return
	}
	var (
		url, secret    string
		events, scopes []string
	)
	for name, dst := range map[string]any{"url": &url, "secret": &secret, "events": &events, e.scopeField(): &scopes} {
		if _, ok := field(w, fields, name, dst); !ok {
			return
		}
	}
	if err := checkCallbackURL(url); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := CheckSecret(secret); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sub, err := e.notify.Subs().Create(r.Context(), principal, url, secret, events, scopes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := e.subscriptionView(sub)
	// The only time the secret is returned.
	view["secret"] = sub.Secret
	writeJSON(w, http.StatusCreated, view)
}

func (e *Engine) patchOwnSubscription(w http.ResponseWriter, r *http.Request, principal, id string) {
	fields, ok := readSubscriptionBody(w, r)
	if !ok {
		return
	}
	p, err := decodePatch(fields, e.scopeField())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	found, err := e.notify.Subs().Update(r.Context(), principal, id, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such subscription", http.StatusNotFound)
		return
	}
	e.getOwnSubscription(w, r, principal, id)
}

// decodePatch reads the fields a patch sets. A field that is absent stays
// nil, so the store leaves it alone.
func decodePatch(fields map[string]json.RawMessage, scopeField string) (SubscriptionPatch, error) {
	var p SubscriptionPatch
	decode := func(name string, dst any) (bool, error) {
		raw, ok := fields[name]
		if !ok {
			return false, nil
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return false, errors.New(name + ": " + err.Error())
		}
		return true, nil
	}
	var url, secret string
	var events, scopes []string
	var active bool
	if ok, err := decode("url", &url); err != nil {
		return p, err
	} else if ok {
		if err := checkCallbackURL(url); err != nil {
			return p, err
		}
		p.URL = &url
	}
	if ok, err := decode("secret", &secret); err != nil {
		return p, err
	} else if ok {
		if secret == "" {
			return p, errBadCallback("a patch cannot clear the secret")
		}
		if err := CheckSecret(secret); err != nil {
			return p, err
		}
		p.Secret = &secret
	}
	if ok, err := decode("events", &events); err != nil {
		return p, err
	} else if ok {
		p.Events = &events
	}
	if ok, err := decode(scopeField, &scopes); err != nil {
		return p, err
	} else if ok {
		p.Scopes = &scopes
	}
	if ok, err := decode("active", &active); err != nil {
		return p, err
	} else if ok {
		p.Active = &active
	}
	return p, nil
}
