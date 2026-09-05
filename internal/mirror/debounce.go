package mirror

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// maxDebounceWindow caps the hold, since every eligible read waits it out.
const maxDebounceWindow = 30 * time.Second

// Debouncer makes identical concurrent passthrough READS share call.
type Debouncer struct {
	window time.Duration

	mu     sync.Mutex
	flight map[string]*shared
}

// shared is upstream call several callers are waiting on.
type shared struct {
	done   chan struct{}
	status int
	header http.Header
	body   []byte
}

// NewDebouncer returns a coalescer, or nil for a window: forwarding
// immediately is a real choice, and nil has nothing to hold with.
func NewDebouncer(window time.Duration) *Debouncer {
	if window <= 0 {
		return nil
	}
	return &Debouncer{window: window, flight: map[string]*shared{}}
}

// Share lets an identical concurrent request wait for answer instead of
// making its own. Passthrough is the traffic nothing else protects, and
// consumers asking the same unmodelled question at should cost the budget
// answer rather than.
//
// Only a safe method with no credential of its own qualifies:
// callers holding different credentials ask different questions even at
// the same URL, and answering with the other's data is the reveal layer
// defeated by a cache key.
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
			// The caller left; the shared call carries on for the rest.
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

	// How long a follower may still join. Holding it this long and no longer is
	// what makes this a debounce and not a cache: it stores nothing.
	time.AfterFunc(d.window, func() {
		d.mu.Lock()
		if d.flight[key] == leader {
			delete(d.flight, key)
		}
		d.mu.Unlock()
	})
}

// shareable reports whether callers asking this are asking the same thing.
func shareable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	// A credential makes the answer that caller's own, and never shareable.
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

// bufferedWriter collects answer so it can be handed to every waiter.
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
