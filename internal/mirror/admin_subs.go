package mirror

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxSubscriptionBody caps a registration. Nothing legitimate here is large.
const maxSubscriptionBody = 64 << 10

// subscriptionRequest is what a consumer posts to register.
type subscriptionRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// subscriptionCreated is the and only time the secret is returned.
type subscriptionCreated struct {
	Subscription
	// Secret signs every notification, and is shown here exactly.
	Secret string `json:"secret"`
}

func (a *Admin) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs := a.engine.notify.Subs()
	if subs == nil {
		http.Error(w, "this spec declares no <notify>", http.StatusNotImplemented)
		return
	}
	all, err := subs.All(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if all == nil {
		all = []Subscription{}
	}
	writeJSON(w, http.StatusOK, all)
}

func (a *Admin) createSubscription(w http.ResponseWriter, r *http.Request) {
	subs := a.engine.notify.Subs()
	if subs == nil {
		http.Error(w, "this spec declares no <notify>", http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionBody))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var req subscriptionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "the body is not JSON", http.StatusBadRequest)
		return
	}
	if err := checkCallbackURL(req.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	principal := principalOf(r)
	if principal == "" {
		principal = "operator"
	}
	sub, err := subs.Create(r.Context(), principal, req.URL, req.Events)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, subscriptionCreated{Subscription: sub, Secret: sub.Secret})
}

func (a *Admin) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	subs := a.engine.notify.Subs()
	if subs == nil {
		http.Error(w, "this spec declares no <notify>", http.StatusNotImplemented)
		return
	}
	principal := principalOf(r)
	if principal == "" {
		principal = "operator"
	}
	gone, err := subs.Delete(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !gone {
		http.Error(w, "no such subscription", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkCallbackURL refuses a destination the mirror must not be made to post to.
//
// A registration names where this server will send a signed request, which
// makes an unchecked a request forgery with the mirror's own network
// position behind it. The scheme check is the part that matters: file: and
// gopher: are not endpoints, they are ways to make a client do something else.
func checkCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errBadCallback("a subscription must name a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errBadCallback("the url does not parse")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errBadCallback("a subscription url must be http or https")
	}
	if u.Host == "" {
		return errBadCallback("the url names no host")
	}
	return nil
}

// errBadCallback is a refusal a caller can read and act on.
type errBadCallback string

func (e errBadCallback) Error() string { return string(e) }
