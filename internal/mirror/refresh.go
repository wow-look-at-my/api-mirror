package mirror

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Refresher keeps stored keys warm without a consumer asking first.
type Refresher struct {
	engine   *Engine
	interval time.Duration
	kinds    []string

	mu      sync.Mutex
	last    time.Time
	cycles  int
	errors  int
	swept   int
	running bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewRefresher builds the sweep the spec declared. A spec that declares none
// gets a Refresher that never runs, rather than a nil the callers must check.
func NewRefresher(e *Engine) *Refresher {
	r := &Refresher{engine: e, stop: make(chan struct{})}
	if e.spec.Refresh == nil {
		return r
	}
	r.interval = e.spec.Refresh.Interval
	r.kinds = e.spec.Refresh.Kinds
	if len(r.kinds) == 0 {
		// A resource nobody reads has nothing to keep warm, so the default is
		// every resource some route actually serves.
		for _, rt := range e.spec.Routes {
			kind := routeKind(rt)
			if !slices.Contains(r.kinds, kind) {
				r.kinds = append(r.kinds, kind)
			}
		}
	}
	return r
}

// Enabled reports whether this sweep does anything.
func (r *Refresher) Enabled() bool {
	return r != nil && r.interval > 0 && len(r.kinds) > 0
}

// Start runs the sweep until Stop.
//
// It keeps a key current when nobody is reading it, which is exactly when a
// lost delivery goes unnoticed. The first cycle runs immediately: a restart is
// itself a window deliveries were missed in.
func (r *Refresher) Start() {
	if !r.Enabled() {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.cycle()
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.cycle()
			}
		}
	}()
}

// Stop ends the sweep and waits for the cycle in flight.
func (r *Refresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stop)
	r.mu.Unlock()
	r.wg.Wait()
}

// cycle refreshes every key of every declared kind that has aged out.
func (r *Refresher) cycle() {
	ctx, cancel := context.WithTimeout(context.Background(), refreshCycleTimeout)
	defer cancel()

	swept, failed := 0, 0
	for _, kind := range r.kinds {
		select {
		case <-r.stop:
			return
		default:
		}
		keys, err := r.engine.store.StaleKeys(ctx, kind, time.Now())
		if err != nil {
			logf("refresh: list %s: %v", kind, err)
			failed++
			continue
		}
		for _, k := range keys {
			if err := r.refreshOne(ctx, kind, k); err != nil {
				logf("refresh %s/%s: %v", kind, k.Key, err)
				failed++
				continue
			}
			swept++
		}
	}

	r.mu.Lock()
	r.cycles++
	r.swept += swept
	r.errors += failed
	r.last = time.Now()
	r.mu.Unlock()
}

// refreshCycleTimeout is a leak guard: an overrun belongs in the log.
const refreshCycleTimeout = 30 * time.Minute

// refreshOne re-fetches one key from the plan its freshness row recorded. The
// sweep holds no caller's credential, so a key that needed one stays as it is
// rather than being refetched under the wrong identity.
func (r *Refresher) refreshOne(ctx context.Context, kind string, k StaleKey) error {
	plan, ok := r.engine.planFor(kind, k)
	if !ok {
		return nil
	}
	ctx = withLane(withPlan(ctx, plan), LaneRefresh, "", kind)
	ctx = withForward(ctx, nil)
	_, err := r.engine.fresh.Refresh(ctx, kind, k.Key)
	return err
}

// RefreshStats is what the sweep can say about itself.
type RefreshStats struct {
	Enabled  bool      `json:"enabled"`
	Interval string    `json:"interval,omitempty"`
	Kinds    []string  `json:"kinds,omitempty"`
	Cycles   int       `json:"cycles"`
	Swept    int       `json:"swept"`
	Errors   int       `json:"errors"`
	Last     time.Time `json:"last,omitempty"`
}

// Stats reports the sweep's own state, errors included. A sweep that has been
// failing every cycle for a day looks exactly like a healthy one from the
// outside, so the failure count is on the page rather than only in the log.
func (r *Refresher) Stats() RefreshStats {
	if r == nil {
		return RefreshStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := RefreshStats{
		Enabled: r.Enabled(),
		Kinds:   r.kinds,
		Cycles:  r.cycles,
		Swept:   r.swept,
		Errors:  r.errors,
		Last:    r.last,
	}
	if r.interval > 0 {
		s.Interval = r.interval.String()
	}
	return s
}
