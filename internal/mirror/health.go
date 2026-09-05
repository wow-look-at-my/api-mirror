package mirror

import (
	"net/http"
)

// health answers the declared liveness paths, and reports whether it did.
//
// An unrouted path falls through to the proxy rather than 404ing, and a
// checker reading any non- as "implemented" then calls a healthy container
// unhealthy forever. Status only, and outside the token: a checker has none.
func (e *Engine) health(w *recorder, r *http.Request) bool {
	rule := e.spec.Health
	if rule == nil {
		return false
	}
	switch r.URL.Path {
	case rule.Live:
		e.answerLive(w, r)
		return true
	case rule.PreUpdate:
		e.answerPreUpdate(w, r)
		return true
	default:
		return false
	}
}

// answerLive reports whether this process can still do its job.
//
// It pings the database rather than answering unconditionally. A process whose
// store has gone away still accepts connections and still answers to a
// handler that only proves the goroutine is scheduled.
func (e *Engine) answerLive(w *recorder, r *http.Request) {
	w.note(DispAdmin, "health", "", "live")
	if err := e.store.Ping(r.Context()); err != nil {
		logf("health: %v", err)
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// answerPreUpdate holds while a detached fetch is in flight. A restart inside
// that short window drops the write; refusing costs a refetch instead.
func (e *Engine) answerPreUpdate(w *recorder, r *http.Request) {
	w.note(DispAdmin, "health", "", "pre-update")
	if e.fresh.Busy() {
		http.Error(w, "a fetch is in flight", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
