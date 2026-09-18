package mirror

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// operatorPrincipal owns the subscriptions made through the dashboard. The
// dashboard token already shows the operator every stored row, so their
// subscriptions are not gated.
const operatorPrincipal = "operator"

// visibleTo is the fan-out's gate: may this principal hear about this delivery.
func (e *Engine) visibleTo(ctx context.Context, principal string, d *Delivery) bool {
	res, ok := e.store.Resource(d.Event.Resource)
	if !ok {
		return false
	}
	key, err := ingestKey(res, d.Event, d.Payload)
	if err != nil {
		logf("notify: gate %s: %v", d.ID, err)
		return false
	}
	return e.reveal.Visible(ctx, principal, res, key)
}

// subscriptionPath reports whether a path is the declared self-service prefix.
func (e *Engine) subscriptionPath(path string) bool {
	if e.notify == nil || e.spec.Notify == nil || e.spec.Notify.Path == "" {
		return false
	}
	base := strings.TrimSuffix(e.spec.Notify.Path, "/")
	return path == base || strings.HasPrefix(path, base+"/")
}

// serveSubscriptions is a caller managing their own subscriptions with their
// own credential.
func (e *Engine) serveSubscriptions(rec *recorder, r *http.Request) {
	rec.note(DispAdmin, e.spec.Notify.Path, "", "subscriptions")
	principal, refused := e.ids.resolve(r.Context(), r)
	if refused != nil {
		e.writeMessage(rec, refused.status, refused.message)
		return
	}
	if principal == "" {
		e.writeMessage(rec, http.StatusUnauthorized, "a subscription belongs to a caller: send a credential")
		return
	}
	rec.principal = principal
	subs := e.notify.Subs()
	base := strings.TrimSuffix(e.spec.Notify.Path, "/")
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, base), "/")

	switch {
	case id == "" && r.Method == http.MethodGet:
		own, err := subs.ByPrincipal(r.Context(), principal)
		if err != nil {
			http.Error(rec, err.Error(), http.StatusInternalServerError)
			return
		}
		if own == nil {
			own = []Subscription{}
		}
		writeJSON(rec, http.StatusOK, own)
	case id == "" && r.Method == http.MethodPost:
		createSubscriptionFor(rec, r, subs, principal)
	case id != "" && r.Method == http.MethodDelete:
		gone, err := subs.Delete(r.Context(), principal, id)
		if err != nil {
			http.Error(rec, err.Error(), http.StatusInternalServerError)
			return
		}
		if !gone {
			http.Error(rec, "no such subscription", http.StatusNotFound)
			return
		}
		rec.WriteHeader(http.StatusNoContent)
	case id != "" && r.Method == http.MethodPatch:
		e.patchSubscription(rec, r, subs, principal, id)
	default:
		http.Error(rec, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// patchSubscription re-enables a parked subscription. Active is the only
// field a caller may change; the url and events are fixed at creation.
func (e *Engine) patchSubscription(w http.ResponseWriter, r *http.Request, subs *Subscriptions, principal, id string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req struct {
		Active *bool `json:"active"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Active == nil || !*req.Active {
		http.Error(w, `the only change a subscription takes is {"active": true}`, http.StatusBadRequest)
		return
	}
	found, err := subs.Reactivate(r.Context(), principal, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "no such subscription", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
