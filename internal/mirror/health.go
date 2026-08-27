package mirror

import (
	"net/http"
)

// The liveness surface.
//
// Registration is load-bearing in a way that is easy to miss. An unrouted path
// here does not 404 -- it falls through to the passthrough proxy and answers
// whatever the upstream says about it. A checker that reads any non-404 as
// "implemented" then reports a perfectly healthy container as permanently
// unhealthy, and the cause is a path nobody declared rather than anything
// wrong with the mirror.
//
// These answer with a status and nothing else, and they sit OUTSIDE the
// dashboard token: a checker has no credential to give.

// health answers the declared liveness paths, and reports whether it did.
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
// store has gone away still accepts connections and still answers 200 to a
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

// answerPreUpdate holds while a detached fetch is in flight.
//
// A restart during one loses the fetch and, with it, whatever the answer was
// about to become. The window is short; refusing inside it is the difference
// between a redeploy that costs a refetch and one that drops the write.
func (e *Engine) answerPreUpdate(w *recorder, r *http.Request) {
	w.note(DispAdmin, "health", "", "pre-update")
	if e.fresh.Busy() {
		http.Error(w, "a fetch is in flight", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
