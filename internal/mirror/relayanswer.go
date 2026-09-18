package mirror

import "net/http"

// relayedHeaders are the upstream headers a relayed answer keeps. A client
// reads the status and body to learn what went wrong, and the budget and
// retry headers to learn when it may ask again.
func (e *Engine) relayedHeaders() []string {
	rate := e.spec.Upstream.Rate
	names := []string{"Content-Type", e.spec.Upstream.RetryAfter, "Retry-After",
		rate.Limit, rate.Remaining, rate.Used, rate.Reset, rate.Resource}
	out := names[:0]
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// relayAnswer hands an unstored upstream answer to the caller that earned it,
// status and body unchanged.
func (e *Engine) relayAnswer(w http.ResponseWriter, a *Answer) {
	for _, name := range e.relayedHeaders() {
		if v := a.Header.Values(name); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(name)] = v
		}
	}
	w.Header().Set("X-Mirror-Cache", "relayed")
	w.WriteHeader(a.Status)
	if _, err := w.Write(a.Body); err != nil {
		logf("write relayed answer: %v", err)
	}
}
