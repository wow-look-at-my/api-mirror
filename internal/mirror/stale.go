// stale.go holds OutcomeStale.

package mirror

import (
	"context"
	"net/http"
	"time"
)

// OutcomeStale is an answer served from a row the mirror failed to refresh.
const OutcomeStale Outcome = "stale"

// serveStale answers from the stored row after a failed refresh, and
// reports whether it did. A caller already passed the reveal layer, so the
// row is theirs to see. The stale header and the fetch time say plainly
// what the answer is. A key with nothing stored still gets the error.
func (e *Engine) serveStale(ctx context.Context, w *recorder, m *match, res *Resource, kind, key string, cause error) bool {
	if !res.Store.answersJSON() {
		// A stale signed URL no longer opens, and nothing here marks a stale diff.
		return false
	}
	doc, err := e.read(ctx, m, res)
	if err != nil || doc == nil {
		return false
	}
	if list, ok := doc.([]any); ok && len(list) == 0 {
		// An empty list is indistinguishable from "never fetched".
		return false
	}
	logf("serving stale %s %s: refresh failed: %v", kind, key, cause)
	w.note(DispHit, "", "", "stale")
	w.Header().Set(e.hdr.Stale, "true")
	if meta, err := e.store.Freshness(ctx, kind, key); err == nil && meta != nil && !meta.FetchedAt.IsZero() {
		w.Header().Set(e.hdr.LastFetched, meta.FetchedAt.UTC().Format(time.RFC3339))
	}
	e.write(w, http.StatusOK, doc, OutcomeStale)
	return true
}
