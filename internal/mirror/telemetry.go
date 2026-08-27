package mirror

import (
	"net/http"
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
}

// LaneTally is the running cost of one lane of traffic.
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

// Observe records one outbound exchange everywhere it belongs.
func (t *Telemetry) Observe(e Exchange) {
	if t == nil {
		return
	}
	t.Timeline.Observe(e)

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
	t.upBytes += e.Bytes
}

// ObserveHeaders reads a budget out of an answer's headers.
func (t *Telemetry) ObserveHeaders(principal string, h http.Header) {
	if t == nil {
		return
	}
	t.Rates.Observe(principal, h)
}

// Lanes reports the per-lane cost, busiest first.
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
