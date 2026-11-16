// telemetry.go holds Telemetry.

package mirror

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// Telemetry is everything the mirror knows about its own traffic.
type Telemetry struct {
	Timeline *Timeline
	Requests *RequestLog
	Rates    *RateMeter
	Started  time.Time

	mu      sync.Mutex
	lanes   map[Lane]*LaneTally
	upBytes int

	// onExchange and onRequest are a host's taps.
	onExchange func(Exchange)
	onRequest  func(Request)

	// seen is when each principal last asked this mirror anything.
	seen map[string]time.Time
}

// Tap hands every outbound exchange and every inbound request to a host as
// well. Call it before the engine serves or starts.
func (t *Telemetry) Tap(outbound func(Exchange), inbound func(Request)) {
	t.onExchange, t.onRequest = outbound, inbound
}

// Record logs an inbound request and hands it to the host's tap.
func (t *Telemetry) Record(r Request) {
	t.Requests.Record(r)
	if t.onRequest != nil {
		t.onRequest(r)
	}
}

// seenMax bounds the last-seen map.
const seenMax = 10000

// Seen records a principal's request.
func (t *Telemetry) Seen(principal string, at time.Time) {
	if t == nil || principal == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]time.Time{}
	}
	t.seen[principal] = at
	if len(t.seen) <= seenMax {
		return
	}
	times := make([]time.Time, 0, len(t.seen))
	for _, v := range t.seen {
		times = append(times, v)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	cutoff := times[len(times)/2]
	for p, v := range t.seen {
		if v.Before(cutoff) {
			delete(t.seen, p)
		}
	}
}

// LastSeen copies the last-seen map.
func (t *Telemetry) LastSeen() map[string]time.Time {
	out := map[string]time.Time{}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for p, v := range t.seen {
		out[p] = v
	}
	return out
}

// LaneTally is the running cost of lane of traffic.
type LaneTally struct {
	Lane       Lane          `json:"lane"`
	Count      int           `json:"count"`
	Errors     int           `json:"errors"`
	Bytes      int           `json:"bytes"`
	TotalNanos int64         `json:"-"`
	Mean       time.Duration `json:"mean_ns"`
	Last       time.Time     `json:"last"`
}

// NewTelemetry builds the stores. It is never behind a flag: a view an
// operator must discover and enable is a view nobody has.
func NewTelemetry(rate RateHeaders) *Telemetry {
	return &Telemetry{
		Timeline: NewTimeline(),
		Requests: NewRequestLog(),
		Rates:    NewRateMeter(rate),
		Started:  time.Now(),
		lanes:    map[Lane]*LaneTally{},
	}
}

// Observe records outbound exchange everywhere it belongs.
func (t *Telemetry) Observe(e Exchange) {
	if t == nil {
		return
	}
	t.Timeline.Observe(e)
	// A delivery is inbound, timed at the handler. The tap is for outbound only.
	if t.onExchange != nil && e.Lane != LaneDelivery {
		t.onExchange(e)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	tally, ok := t.lanes[e.Lane]
	if !ok {
		tally = &LaneTally{Lane: e.Lane}
		t.lanes[e.Lane] = tally
	}
	tally.Count++
	tally.Bytes += e.Bytes
	tally.TotalNanos += int64(e.Duration)
	tally.Last = e.Started
	if e.Err != nil || e.Status >= 500 {
		tally.Errors++
	}
	if e.Lane != LaneInbound && e.Lane != LaneDelivery {
		t.upBytes += e.Bytes
	}
}

// ObserveHeaders reads a budget out of an answer's headers.
func (t *Telemetry) ObserveHeaders(principal string, h http.Header) {
	if t == nil {
		return
	}
	t.Rates.Observe(principal, h)
}

// Lanes reports the per-lane cost, busiest.
func (t *Telemetry) Lanes() []LaneTally {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]LaneTally, 0, len(t.lanes))
	for _, tally := range t.lanes {
		copied := *tally
		if copied.Count > 0 {
			copied.Mean = time.Duration(copied.TotalNanos / int64(copied.Count))
		}
		out = append(out, copied)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Count > out[i].Count {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// UpstreamBytes is everything the mirror pulled from the upstream this run. It
// is the number that says whether the cache is earning its keep.
func (t *Telemetry) UpstreamBytes() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.upBytes
}
