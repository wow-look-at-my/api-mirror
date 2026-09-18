package mirror

import "net/http"

// answerRate serves the declared budget path from the passive meter.
//
// Asking the upstream for a budget spends the budget being asked about, and a
// cached copy of that answer is stale by the time anyone reads it. The meter
// already holds the caller's own standing from the headers of every answer
// the mirror received for them. A caller never observed is forwarded, and the
// answer to that is observed in turn.
func (e *Engine) answerRate(w *recorder, r *http.Request) bool {
	path := e.spec.Upstream.Rate.Answer
	if path == "" || r.Method != http.MethodGet || r.URL.Path != path {
		return false
	}
	principal := principalOf(r)
	resources := map[string]any{}
	for _, b := range e.tel.Rates.Snapshot() {
		if b.Principal != principal || b.Stale {
			continue
		}
		reading := map[string]any{
			"limit":     b.Limit,
			"remaining": b.Remaining,
			"used":      b.Used,
			"resource":  b.Resource,
		}
		if !b.Reset.IsZero() {
			reading["reset"] = b.Reset.Unix()
		}
		resources[b.Resource] = reading
	}
	if len(resources) == 0 {
		e.passthrough(w, r, PassUnobservedBudget)
		return true
	}
	doc := map[string]any{"resources": resources}
	if core, ok := resources["core"]; ok {
		doc["rate"] = core
	}
	w.note(DispHit, path, "", "rate-meter")
	w.Header().Set("Cache-Control", "no-store")
	e.write(w, http.StatusOK, doc, OutcomeHit)
	return true
}
