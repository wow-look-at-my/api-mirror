package mirror

import (
	"io"
	"net/http"
	"strings"
)

// relayHeaders are what a relay forwards. Authorization is absent on
// purpose: a foreign host must not receive what was meant for the upstream.
var relayHeaders = []string{"Content-Type", "Accept", "Accept-Encoding"}

// matchRelay finds the declared relay this request names.
func (e *Engine) matchRelay(r *http.Request) (*Relay, bool) {
	for _, rl := range e.spec.Relays {
		if rl.Method == r.Method && rl.Path == r.URL.EscapedPath() {
			return rl, true
		}
	}
	return nil, false
}

// relay forwards a body to a fixed foreign URL and answers with what comes
// back. Nothing is stored and nothing is keyed, so this is a passthrough that
// happens to leave the upstream's host.
//
// The mirror's own CORS headers are already on the answer, and the foreign
// host's are dropped: corsMiddleware is the single authority, and a duplicate
// allow-origin makes a browser refuse the response outright.
func (e *Engine) relay(w *recorder, r *http.Request, rl *Relay) {
	w.note(DispPassthrough, rl.Path, "", string(PassRelay))
	w.Header().Set("X-Mirror-Cache", "passthrough")
	w.Header().Set("X-Mirror-Passthrough-Reason", string(PassRelay))

	ctx := withLane(r.Context(), LanePassthrough, w.principal, string(PassRelay))
	req, err := http.NewRequestWithContext(ctx, rl.Method, rl.To, r.Body)
	if err != nil {
		http.Error(w, "relay: bad destination", http.StatusBadGateway)
		return
	}
	req.ContentLength = r.ContentLength
	for _, name := range relayHeaders {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}

	resp, err := e.up.client.Do(req)
	if err != nil {
		logf("relay %s: %v", rl.Path, err)
		http.Error(w, "relay: upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		if strings.HasPrefix(strings.ToLower(name), "access-control-allow-") {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logf("relay %s: copying the answer: %v", rl.Path, err)
	}
}
