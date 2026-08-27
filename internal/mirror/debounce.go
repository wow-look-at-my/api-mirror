package mirror

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// maxDebounceWindow caps the hold. Every eligible read waits this long, so a
// fat-fingered "5m" must fail at load rather than wedge the API for an hour
// before anybody notices what changed.
const maxDebounceWindow = 30 * time.Second

// Debouncer makes identical concurrent passthrough READS share one upstream
// call.
//
// A passthrough is the traffic the spec does not model yet, which is exactly
// the traffic nothing else is protecting. Ten consumers asking the same
// unmodelled question at once should cost the budget one answer, not ten.
type Debouncer struct {
	window time.Duration

	mu     sync.Mutex
	flight map[string]*shared
}

// shared is one upstream call several callers are waiting on.
type shared struct {
	done   chan struct{}
	status int
	header http.Header
	body   []byte
}

// NewDebouncer returns a coalescer. A zero window returns nil: forwarding
// immediately is a real choice, and a nil Debouncer says so by having nothing
// to hold with.
func NewDebouncer(window time.Duration) *Debouncer {
	if window <= 0 {
		return nil
	}
	return &Debouncer{window: window, flight: map[string]*shared{}}
}

// Share forwards one request, letting an identical concurrent request wait for
// the same answer instead of making its own.
//
// Only a safe method with no credential of its own is eligible. Two callers
// holding different credentials are asking different questions even when the
// URL matches, and handing one caller the other's answer would be the reveal
// layer defeated by a cache key.
func (d *Debouncer) Share(w *recorder, r *http.Request, forward func(http.ResponseWriter, *http.Request)) {
	if d == nil || !shareable(r) {
		forward(w, r)
		return
	}
	key := r.Method + " " + r.URL.String()

	d.mu.Lock()
	if inflight, ok := d.flight[key]; ok {
		d.mu.Unlock()
		select {
		case <-inflight.done:
			w.Header().Set("X-Mirror-Debounced", "true")
			replay(w, inflight)
		case <-r.Context().Done():
			// The caller left. Nothing to write, and the shared call carries on
			// for whoever is still waiting.
		}
		return
	}
	leader := &shared{done: make(chan struct{})}
	d.flight[key] = leader
	d.mu.Unlock()

	buf := &bufferedWriter{header: http.Header{}}
	forward(buf, r)

	leader.status = buf.status
	leader.header = buf.header
	leader.body = buf.body.Bytes()
	close(leader.done)

	replay(w, leader)

	// The window is how long a follower may still join. Holding the entry that
	// long and no longer is what makes this a debounce rather than a cache: it
	// coalesces what is happening now, and stores nothing.
	time.AfterFunc(d.window, func() {
		d.mu.Lock()
		if d.flight[key] == leader {
			delete(d.flight, key)
		}
		d.mu.Unlock()
	})
}

// shareable reports whether two callers asking this are asking the same thing.
func shareable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	// A credential makes the answer that caller's own. Sharing it across callers
	// would hand one caller's data to another, which is the one failure this
	// whole engine exists to make impossible.
	return r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == ""
}

func replay(w http.ResponseWriter, s *shared) {
	for k, vs := range s.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if _, err := w.Write(s.body); err != nil {
		logf("debounce: replay: %v", err)
	}
}

// bufferedWriter collects one answer so it can be handed to every waiter.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
