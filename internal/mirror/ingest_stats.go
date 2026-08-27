package mirror

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// What the ingest endpoint can say about itself, and who it tells after a
// delivery lands.
//
// A lost delivery is the quietest failure a mirror has: every cache it would
// have moved keeps serving its last absorbed answer for the whole TTL, and
// nothing reports a gap. Counting what arrives is the only way an operator sees
// the shape of that silence -- "no pushes for nine hours" only reads as wrong
// next to what a normal hour looks like.

// SetTelemetry puts every delivery on the chart, timed. A delivery is traffic
// like any other, and the rule here is that everything the mirror exchanges is
// visible.
func (i *Ingest) SetTelemetry(tel *Telemetry) { i.tel = tel }

// SetNotifier installs the fan-out that runs after a delivery is applied.
func (i *Ingest) SetNotifier(n *Notifier) { i.notifier = n }

// ServeHTTP receives one delivery and records what it cost.
//
// The disposition is read back off the response rather than returned by the
// handler, so a path that answers early is counted the same as one that runs to
// the end. An uncounted refusal is exactly the delivery an operator needs.
func (i *Ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	probe := &dispositionProbe{ResponseWriter: w}
	i.serve(probe, r)

	disp := probe.disposition()
	i.stats.record(r.Header.Get(i.events.TypeHeader), disp)
	i.tel.Observe(Exchange{
		Lane:     LaneDelivery,
		Method:   r.Method,
		Path:     r.URL.Path,
		Status:   probe.status,
		Bytes:    probe.bytes,
		Started:  started,
		Duration: time.Since(started),
		Detail:   string(disp),
	})
}

// dispositionProbe reads what the ingest handler actually answered.
type dispositionProbe struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (p *dispositionProbe) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
		p.ResponseWriter.WriteHeader(status)
	}
}

func (p *dispositionProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	n, err := p.ResponseWriter.Write(b)
	p.bytes += n
	return n, err
}

// disposition reports what the handler said, naming the refusals the handler
// answers with a plain status rather than a disposition.
func (p *dispositionProbe) disposition() DeliveryDisposition {
	if d := p.Header().Get(dispositionHeader); d != "" {
		return DeliveryDisposition(d)
	}
	switch p.status {
	case http.StatusForbidden:
		return "unverified"
	case http.StatusBadRequest:
		return "unparseable"
	default:
		return "rejected"
	}
}

// deliveryStats tallies what has arrived.
type deliveryStats struct {
	mu     sync.Mutex
	total  int
	byType map[string]int
	byDisp map[DeliveryDisposition]int
	last   time.Time
	first  time.Time
}

func (s *deliveryStats) record(typ string, d DeliveryDisposition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byType == nil {
		s.byType = map[string]int{}
		s.byDisp = map[DeliveryDisposition]int{}
		s.first = time.Now()
	}
	if typ == "" {
		typ = "(none)"
	}
	s.total++
	s.byType[typ]++
	s.byDisp[d]++
	s.last = time.Now()
}

// DeliveryStats is what the ingest endpoint reports to the dashboard.
type DeliveryStats struct {
	Total        int                         `json:"total"`
	Types        []TypeCount                 `json:"types"`
	Dispositions map[DeliveryDisposition]int `json:"dispositions"`
	Last         time.Time                   `json:"last,omitempty"`
	Since        time.Time                   `json:"since,omitempty"`
	// Declared is every event type the spec models, so a type that has never
	// arrived is visible as a zero rather than as an absent row. A subscription
	// nobody set up looks identical to a quiet week without this.
	Declared []string `json:"declared"`
	Window   string   `json:"reorder_window"`
}

// TypeCount is how many deliveries of one type arrived.
type TypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// Stats reports what the ingest endpoint has seen.
func (i *Ingest) Stats() DeliveryStats {
	if i == nil {
		return DeliveryStats{}
	}
	i.stats.mu.Lock()
	defer i.stats.mu.Unlock()

	out := DeliveryStats{
		Total:        i.stats.total,
		Dispositions: map[DeliveryDisposition]int{},
		Last:         i.stats.last,
		Since:        i.stats.first,
		Window:       i.window.String(),
	}
	for d, n := range i.stats.byDisp {
		out.Dispositions[d] = n
	}
	for t, n := range i.stats.byType {
		out.Types = append(out.Types, TypeCount{Type: t, Count: n})
	}
	sort.Slice(out.Types, func(a, b int) bool { return out.Types[a].Count > out.Types[b].Count })
	for _, ev := range i.events.List {
		out.Declared = append(out.Declared, ev.Type)
	}
	sort.Strings(out.Declared)
	return out
}

// applyAndNotify applies one delivery and then tells the subscribers.
//
// The order is the point. A subscriber told before the write lands would come
// back and read the state the delivery was about to replace, which is the exact
// race the notification exists to remove.
func (i *Ingest) applyAndNotify(ctx context.Context, d *Delivery) (DeliveryDisposition, error) {
	disp, err := i.apply(ctx, d)
	if err != nil || i.notifier == nil {
		return disp, err
	}
	if disp == DeliveryApplied || disp == DeliveryInvalidated {
		i.notifier.Fan(ctx, d, disp)
	}
	return disp, nil
}
