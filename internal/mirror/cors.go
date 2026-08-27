package mirror

import (
	"net/http"
	"strconv"
	"strings"
)

// CORS: what a browser is allowed to do with this mirror.
//
// Allowing any origin is safe here in a way it usually is not, and the reason
// is the reveal layer: what a caller may see is decided by proving THEIR access
// upstream, not by where the page asking came from. An origin list would gate a
// door that is not the door.

// answerPreflight handles an OPTIONS request, and reports whether it did.
//
// A preflight is answered without authentication on purpose. It carries no
// credential by definition, so requiring one would refuse every cross-origin
// request before the real request was ever made.
func (e *Engine) answerPreflight(w *recorder, r *http.Request) bool {
	e.setCORS(w, r)
	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	rule := e.spec.CORS
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
		w.Header().Set("Access-Control-Allow-Headers", req)
	}
	if rule.MaxAge > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(rule.MaxAge.Seconds())))
	}
	w.note(DispAdmin, "OPTIONS *", "", "preflight")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// setCORS puts the allow headers on an answer.
//
// Expose-Headers is the half that is easy to forget and impossible to work
// around: without it a browser hides every X-Mirror-* header, so a page cannot
// tell a hit from a passthrough and cannot read the budget it is spending. A
// rebuilt answer has no upstream list to inherit one from either.
func (e *Engine) setCORS(w http.ResponseWriter, r *http.Request) {
	rule := e.spec.CORS
	if rule == nil {
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	allowed := ""
	for _, o := range rule.Origins {
		if o == "*" {
			allowed = "*"
			break
		}
		if strings.EqualFold(o, origin) {
			allowed = origin
			break
		}
	}
	if allowed == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", allowed)
	if allowed != "*" {
		w.Header().Add("Vary", "Origin")
	}
	if len(rule.Expose) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.Expose, ", "))
	}
}

// defaultExposed are the headers a client needs to read its own answer. This
// is the mirror's own vocabulary; the spec names whatever the upstream adds.
var defaultExposed = []string{
	"X-Mirror-Cache",
	"X-Mirror-Passthrough-Reason",
	"X-Mirror-Debounced",
	"X-Mirror-Stale",
	"X-Mirror-Fetched-At",
}
