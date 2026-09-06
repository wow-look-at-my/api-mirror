package mirror

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Replayer reads the upstream's own failure log and asks for those deliveries
// again.
type Replayer struct {
	engine *Engine
	rule   *Replay

	mu      sync.Mutex
	asked   map[string]time.Time
	cycles  int
	found   int
	sent    int
	errors  int
	last    time.Time
	running bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewReplayer builds the replayer a spec declared.
func NewReplayer(e *Engine) *Replayer {
	return &Replayer{engine: e, rule: e.spec.Replay, asked: map[string]time.Time{}, stop: make(chan struct{})}
}

// declared reports whether a spec asked for a replayer at all.
func (r *Replayer) declared() bool {
	return r != nil && r.rule != nil && r.rule.Interval > 0 && r.rule.List != "" && r.rule.Redeliver != ""
}

// Enabled reports whether this replayer does anything.
func (r *Replayer) Enabled() bool {
	return r.declared() && r.missing() == ""
}

// missing names a declared requirement that has no value. A failure log is an
// authenticated endpoint, so without the credential every cycle is a call that
// can only fail, forever: a job that looks busy and recovers nothing.
func (r *Replayer) missing() string {
	if r == nil || r.rule == nil || r.rule.Requires == "" {
		return ""
	}
	if v := lookupPath(r.engine.vars, r.rule.Requires); v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
		return ""
	}
	return r.rule.Requires
}

// Start runs the replayer until Stop. The cycle runs immediately: a
// restart is itself a window deliveries were missed in.
func (r *Replayer) Start() {
	if !r.declared() {
		return
	}
	if want := r.missing(); want != "" {
		logf("replay: OFF because %s is empty. A delivery this mirror never received stays lost, "+
			"and the caches it would have moved serve their last absorbed answer for the whole TTL.", want)
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
		ticker := time.NewTicker(r.rule.Interval)
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

// Stop ends the replayer and waits for the cycle in flight.
func (r *Replayer) Stop() {
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

// cycle reads the failure log and asks for what is still missing.
//
// A lost delivery is the quietest failure a mirror has. The provider sends
// , nothing retries, and every cache that delivery would have moved serves
// its last answer for the whole TTL -- well-formed, recent-looking and wrong.
// A shorter TTL shrinks that window and hides it; it does not close it.
func (r *Replayer) cycle() {
	ctx, cancel := context.WithTimeout(context.Background(), replayCycleTimeout)
	defer cancel()
	ctx = withLane(ctx, LaneReplay, "", "list")

	answer, err := r.engine.up.Call(ctx, "GET", r.rule.List, r.engine.vars, nil, nil)
	if err != nil {
		r.tally(0, 0, 1)
		logf("replay: read the failure log: %v", err)
		return
	}
	if answer.Status < 200 || answer.Status >= 300 {
		r.tally(0, 0, 1)
		logf("replay: the failure log answered %d", answer.Status)
		return
	}
	doc, err := decodeJSON(answer.Body)
	if err != nil {
		r.tally(0, 0, 1)
		logf("replay: the failure log is not JSON: %v", err)
		return
	}
	items, ok := doc.([]any)
	if !ok {
		r.tally(0, 0, 1)
		logf("replay: the failure log is not a list")
		return
	}

	found, sent, failed := 0, 0, 0
	cutoff := time.Now().Add(-r.lookback())
	for _, item := range items {
		if sent >= r.max() {
			// Stated, never silent: a quiet stop at reads as a find of.
			logf("replay: stopped at the %d-per-cycle cap with %d still listed", r.max(), len(items)-found)
			break
		}
		id, at, ok := r.identify(item)
		if !ok {
			continue
		}
		found++
		if !at.IsZero() && at.Before(cutoff) {
			continue
		}
		if r.alreadyAsked(id) {
			// per delivery: asking makes recovery a source of duplicates.
			continue
		}
		if err := r.ask(ctx, item, id); err != nil {
			failed++
			logf("replay %s: %v", id, err)
			continue
		}
		sent++
	}
	r.tally(found, sent, failed)
	if sent > 0 {
		logf("replay: asked for %d of %d listed failures to be re-sent", sent, found)
	}
}

// replayCycleTimeout bounds cycle.
const replayCycleTimeout = 5 * time.Minute

func (r *Replayer) identify(item any) (string, time.Time, bool) {
	id := stringAt(item, r.rule.ID)
	if id == "" {
		return "", time.Time{}, false
	}
	at, _ := timeAt(item, r.rule.At)
	return id, at, true
}

func (r *Replayer) ask(ctx context.Context, item any, id string) error {
	path, err := renderString(r.rule.Redeliver, map[string]any{
		"delivery": item,
		"id":       id,
		"var":      r.engine.vars["var"],
		"env":      r.engine.vars["env"],
	})
	if err != nil {
		return err
	}
	method := r.rule.Method
	if method == "" {
		method = "POST"
	}
	answer, err := r.engine.up.Call(withLane(ctx, LaneReplay, "", "redeliver"), method, path, r.engine.vars, nil, nil)
	if err != nil {
		return err
	}
	if answer.Status < 200 || answer.Status >= 300 {
		return &replayRefused{Status: answer.Status, ID: id}
	}
	r.mu.Lock()
	r.asked[id] = time.Now()
	r.mu.Unlock()
	return nil
}

// replayRefused is the upstream declining to re-send delivery.
type replayRefused struct {
	Status int
	ID     string
}

func (e *replayRefused) Error() string {
	return "the upstream answered " + itoa(e.Status) + " to a redelivery request"
}

func (r *Replayer) alreadyAsked(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.asked[id]
	if !ok {
		return false
	}
	if time.Since(at) > r.lookback() {
		// Past the lookback the failure log will not list it again either.
		delete(r.asked, id)
		return false
	}
	return true
}

func (r *Replayer) lookback() time.Duration {
	if r.rule.Lookback > 0 {
		return r.rule.Lookback
	}
	return 24 * time.Hour
}

func (r *Replayer) max() int {
	if r.rule.Max > 0 {
		return r.rule.Max
	}
	return 25
}

func (r *Replayer) tally(found, sent, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cycles++
	r.found += found
	r.sent += sent
	r.errors += failed
	r.last = time.Now()
}

// ReplayStats is what the replayer reports to the dashboard.
type ReplayStats struct {
	Enabled bool `json:"enabled"`
	// Off names the empty requirement: a decision, not a mystery.
	Off      string    `json:"off,omitempty"`
	Interval string    `json:"interval,omitempty"`
	Cycles   int       `json:"cycles"`
	Found    int       `json:"found"`
	Resent   int       `json:"resent"`
	Errors   int       `json:"errors"`
	Last     time.Time `json:"last,omitempty"`
}

// Stats reports the replayer's own state.
func (r *Replayer) Stats() ReplayStats {
	if r == nil {
		return ReplayStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := ReplayStats{
		Enabled: r.declared() && r.missing() == "",
		Cycles:  r.cycles,
		Found:   r.found,
		Resent:  r.sent,
		Errors:  r.errors,
		Last:    r.last,
	}
	if r.rule != nil && r.rule.Interval > 0 {
		s.Interval = r.rule.Interval.String()
	}
	if r.declared() {
		s.Off = r.missing()
	}
	return s
}
