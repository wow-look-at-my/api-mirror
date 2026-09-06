package mirror

import (
	"net/http"
	"time"
)

// The consistency check's HTTP surface.
//
// GET reports; POST with apply=true repairs. The split is the point: a read
// that could rewrite rows depending on a query parameter is a read nobody can
// run without reading the code.

func (a *Admin) check(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		http.Error(w, "name a kind: the check re-asks the upstream about every key it holds", http.StatusBadRequest)
		return
	}
	repair := r.Method == http.MethodPost && r.URL.Query().Get("apply") == "true"

	if r.URL.Query().Get("stream") == "1" {
		a.streamCheck(w, r, kind, repair)
		return
	}

	summary := CheckSummary{Kind: kind}
	started := time.Now()
	err := a.engine.Check(r.Context(), kind, repair, summary.add)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summary.done(started)
	writeJSON(w, http.StatusOK, summary)
}

// streamCheck writes JSON object per key as it is decided.
//
// A check asks the upstream per stored key, so a large kind takes minutes.
// Buffering it means an operator watches a spinner and cannot tell a slow check
// from a wedged; each line is flushed as it is decided.
func (a *Admin) streamCheck(w http.ResponseWriter, r *http.Request, kind string, repair bool) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	summary := CheckSummary{Kind: kind}
	started := time.Now()

	emit := func(v any) {
		body, err := marshalJSON(v)
		if err != nil {
			return
		}
		w.Write(append(body, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	err := a.engine.Check(r.Context(), kind, repair, func(k KeyCheck) {
		summary.add(k)
		emit(k)
	})
	summary.done(started)
	if err != nil {
		// The header already went out, so there is no status code left to set.
		emit(map[string]string{"error": err.Error()})
		return
	}
	emit(summary)
}
