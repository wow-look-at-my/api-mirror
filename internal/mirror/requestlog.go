package mirror

import (
	"sort"
	"sync"
	"time"
)

// requestLogMax bounds the log. It is a live view of recent traffic, not an
// audit trail, so the oldest entry leaves rather than the newest being refused.
const requestLogMax = 2000

// Disposition is what the mirror DID with one inbound request. The vocabulary
// is closed: a request whose outcome has no name here cannot be accounted for
// on the dashboard.
type Disposition string

const (
	DispHit         Disposition = "hit"         // answered from a fresh row
	DispMiss        Disposition = "miss"        // answered after a fetch
	DispPassthrough Disposition = "passthrough" // forwarded, with a reason
	DispDenied      Disposition = "denied"      // the reveal layer refused
	DispRefusal     Disposition = "refusal"     // an upstream refusal the route absorbed
	DispError       Disposition = "error"       // the mirror could not answer
	DispDelivery    Disposition = "delivery"    // an inbound webhook
	DispAdmin       Disposition = "admin"       // the dashboard's own surface
)

// Request is one inbound request as the mirror handled it.
type Request struct {
	At          time.Time     `json:"at"`
	Method      string        `json:"method"`
	Path        string        `json:"path"`
	Shape       string        `json:"shape"`
	Resource    string        `json:"resource,omitempty"`
	Disposition Disposition   `json:"disposition"`
	Reason      string        `json:"reason,omitempty"`
	Status      int           `json:"status"`
	Bytes       int           `json:"bytes"`
	Duration    time.Duration `json:"duration_ns"`
	Principal   string        `json:"principal,omitempty"`
}

// RequestLog is a bounded ring of recent inbound requests, plus the running
// per-shape tallies the dashboard reads.
//
// The SHAPE is the point. One line per request answers "what happened just
// now"; the tally per route shape answers "what is this mirror actually being
// asked for", which is the question that decides what to model next.
type RequestLog struct {
	mu      sync.Mutex
	entries []Request
	head    int
	size    int
	groups  map[string]*RequestGroup
	now     func() time.Time
}

// RequestGroup is every request that shared one route shape.
type RequestGroup struct {
	Shape        string                 `json:"shape"`
	Method       string                 `json:"method"`
	Resource     string                 `json:"resource,omitempty"`
	Count        int                    `json:"count"`
	Dispositions map[Disposition]int    `json:"dispositions"`
	Reasons      map[string]int         `json:"reasons,omitempty"`
	Bytes        int                    `json:"bytes"`
	TotalNanos   int64                  `json:"-"`
	Last         time.Time              `json:"last"`
	Samples      []string               `json:"samples,omitempty"`
	extra        map[string]interface{} `json:"-"`
}

// MeanDuration is the average an operator reads next to the count. A total with
// no count behind it is a division, not a measurement, so it reports zero.
func (g *RequestGroup) MeanDuration() time.Duration {
	if g.Count == 0 {
		return 0
	}
	return time.Duration(g.TotalNanos / int64(g.Count))
}

// NewRequestLog returns an empty log.
func NewRequestLog() *RequestLog {
	return &RequestLog{
		entries: make([]Request, requestLogMax),
		groups:  map[string]*RequestGroup{},
		now:     time.Now,
	}
}

// Record files one handled request.
func (l *RequestLog) Record(r Request) {
	if l == nil {
		return
	}
	if r.At.IsZero() {
		r.At = l.now()
	}
	if r.Shape == "" {
		r.Shape = r.Path
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size == len(l.entries) {
		l.head = (l.head + 1) % len(l.entries)
		l.size--
	}
	l.entries[(l.head+l.size)%len(l.entries)] = r
	l.size++

	// The tallies outlive the ring on purpose: an operator asking what this
	// mirror is asked for wants the whole run, not the last two thousand lines.
	key := r.Method + " " + r.Shape
	g, ok := l.groups[key]
	if !ok {
		g = &RequestGroup{
			Shape:        r.Shape,
			Method:       r.Method,
			Resource:     r.Resource,
			Dispositions: map[Disposition]int{},
			Reasons:      map[string]int{},
		}
		l.groups[key] = g
	}
	g.Count++
	g.Bytes += r.Bytes
	g.TotalNanos += int64(r.Duration)
	g.Last = r.At
	g.Dispositions[r.Disposition]++
	if r.Reason != "" {
		g.Reasons[r.Reason]++
	}
	if r.Shape != r.Path && len(g.Samples) < 3 && !containsString(g.Samples, r.Path) {
		g.Samples = append(g.Samples, r.Path)
	}
}

// Recent returns the newest entries first, at most n of them.
func (l *RequestLog) Recent(n int) []Request {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > l.size {
		n = l.size
	}
	out := make([]Request, 0, n)
	for i := range n {
		out = append(out, l.entries[(l.head+l.size-1-i+len(l.entries))%len(l.entries)])
	}
	return out
}

// Groups returns the per-shape tallies, busiest first.
func (l *RequestLog) Groups() []*RequestGroup {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*RequestGroup, 0, len(l.groups))
	for _, g := range l.groups {
		copied := *g
		copied.Dispositions = maps(g.Dispositions)
		copied.Reasons = mapsString(g.Reasons)
		copied.Samples = append([]string(nil), g.Samples...)
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Shape < out[j].Shape
	})
	return out
}

func maps(in map[Disposition]int) map[Disposition]int {
	out := make(map[Disposition]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapsString(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
