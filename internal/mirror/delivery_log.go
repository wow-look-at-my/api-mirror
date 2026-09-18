package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wow-look-at-my/go-containers/set"
	"slices"
	"strings"
	"sync"
	"time"
)

// deliveryLogSize bounds the per-delivery log. It is a live view of the most
// recent traffic, reset on restart, never an audit trail.
const deliveryLogSize = 200

// DeliveryRecord is a single line of the delivery log. An arrival is the
// request the provider sent; an apply is a single handler's outcome for it,
// and a delivery the spec fans out has a single apply per resource it moves.
type DeliveryRecord struct {
	At          time.Time           `json:"at"`
	Kind        string              `json:"kind"`
	ID          string              `json:"id,omitempty"`
	Type        string              `json:"type"`
	Action      string              `json:"action,omitempty"`
	Resource    string              `json:"resource,omitempty"`
	Subject     string              `json:"subject,omitempty"`
	Clock       time.Time           `json:"clock,omitzero"`
	LagSeconds  float64             `json:"lag_seconds,omitempty"`
	Status      int                 `json:"status,omitempty"`
	Disposition DeliveryDisposition `json:"disposition"`
	Error       string              `json:"error,omitempty"`
}

// deliveryLog is a ring of the most recent records, oldest overwritten earliest.
type deliveryLog struct {
	mu      sync.Mutex
	ring    []DeliveryRecord
	next    int
	dropped int
}

func (l *deliveryLog) add(r DeliveryRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) < deliveryLogSize {
		l.ring = append(l.ring, r)
		return
	}
	l.ring[l.next] = r
	l.next = (l.next + 1) % deliveryLogSize
	l.dropped++
}

// recent returns the log newest and how many older records it dropped.
func (l *deliveryLog) recent() ([]DeliveryRecord, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]DeliveryRecord, 0, len(l.ring))
	for i := range l.ring {
		out = append(out, l.ring[(l.next+len(l.ring)-1-i)%len(l.ring)])
	}
	return out, l.dropped
}

// orderingStats counts what the watermark and the reorder window did. A
// distribution, not a boolean: a superseded count means little without the
// lag it happened at.
type orderingStats struct {
	mu         sync.Mutex
	ordered    int
	unordered  int
	superseded int
	failed     int
	lagSum     time.Duration
	lagWorst   time.Duration
	held       int
	reordered  int
}

func (s *orderingStats) applied(d *Delivery, disp DeliveryDisposition, received time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case disp == DeliveryFailed:
		s.failed++
	case d.Event.Unordered || d.At.IsZero():
		s.unordered++
	default:
		s.ordered++
		lag := received.Sub(d.At)
		s.lagSum += lag
		s.lagWorst = max(s.lagWorst, lag)
	}
	if disp == DeliverySuperseded {
		s.superseded++
	}
}

func (s *orderingStats) batch(items []*Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held += len(items)
	if !slices.IsSortedFunc(items, func(a, b *Delivery) int { return a.At.Compare(b.At) }) {
		s.reordered++
	}
}

// OrderingView is what the Webhooks tab shows about ordering.
type OrderingView struct {
	Ordered           int     `json:"ordered"`
	Unordered         int     `json:"unordered"`
	Superseded        int     `json:"superseded"`
	Failed            int     `json:"failed"`
	MeanLagSeconds    float64 `json:"mean_lag_seconds"`
	WorstLagSeconds   float64 `json:"worst_lag_seconds"`
	Held              int     `json:"held"`
	Reordered         int     `json:"reordered"`
	ReorderWindowSecs float64 `json:"reorder_window_seconds"`
}

func (s *orderingStats) view(window time.Duration) OrderingView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := OrderingView{
		Ordered:           s.ordered,
		Unordered:         s.unordered,
		Superseded:        s.superseded,
		Failed:            s.failed,
		WorstLagSeconds:   s.lagWorst.Seconds(),
		Held:              s.held,
		Reordered:         s.reordered,
		ReorderWindowSecs: window.Seconds(),
	}
	if s.ordered > 0 {
		v.MeanLagSeconds = s.lagSum.Seconds() / float64(s.ordered)
	}
	return v
}

// recordApply logs a single handler's outcome and counts it for ordering.
func (i *Ingest) recordApply(d *Delivery, disp DeliveryDisposition, err error) {
	rec := DeliveryRecord{
		At:          i.now(),
		Kind:        "apply",
		ID:          d.ID,
		Type:        d.Type,
		Action:      actionOf(d.Payload),
		Resource:    d.Event.Resource,
		Subject:     d.Subject,
		Clock:       d.At,
		Disposition: disp,
	}
	if !d.At.IsZero() && !d.Received.IsZero() {
		rec.LagSeconds = d.Received.Sub(d.At).Seconds()
	}
	if err != nil {
		rec.Error = err.Error()
	}
	i.log.add(rec)
	i.ordering.applied(d, disp, d.Received)
}

func actionOf(payload any) string {
	if a, ok := lookupPath(payload, "action").(string); ok {
		return a
	}
	return ""
}

// EventSubscriptions is the upstream's own statement of which event types it
// sends this mirror. A delivery log can prove an event arrived, never that a
// quiet type is unsubscribed, so the answer is asked for rather than inferred.
type EventSubscriptions struct {
	// Path is the upstream path answering the subscription, asked as the App.
	Path string
	// Field is the path into that answer holding the list of event types.
	Field string
	// Always names types the upstream sends whether or not they are listed.
	Always []string
}

// MissingSubscription is a declared event type the upstream does not send.
type MissingSubscription struct {
	Type      string   `json:"type"`
	Resources []string `json:"resources"`
}

// missingSubscriptions asks the upstream what it sends and reports every
// declared type absent from the answer. It returns nil, never an empty list,
// when the answer cannot be had: reporting everything missing on a failed
// call asserts a problem the mirror has no evidence for.
func (e *Engine) missingSubscriptions(ctx context.Context) ([]MissingSubscription, error) {
	rule := e.spec.Events.Subscriptions
	if rule == nil || e.spec.Upstream.App == nil {
		return nil, nil
	}
	ctx = withAppCall(withLane(ctx, LaneAdmin, "", "subscriptions"))
	answer, err := e.up.Call(ctx, "GET", rule.Path, e.vars, nil, nil)
	if err != nil {
		return nil, err
	}
	if answer.Status < 200 || answer.Status >= 300 {
		return nil, fmt.Errorf("%s answered %d", rule.Path, answer.Status)
	}
	var doc any
	if err := json.Unmarshal(answer.Body, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", rule.Path, err)
	}
	listed, ok := lookupPath(doc, rule.Field).([]any)
	if !ok {
		return nil, fmt.Errorf("%s carries no list at %q", rule.Path, rule.Field)
	}
	have := set.New[string]()
	for _, t := range listed {
		have.Add(fmt.Sprint(t))
	}
	for _, t := range rule.Always {
		have.Add(t)
	}
	byType := map[string][]string{}
	for _, ev := range e.spec.Events.List {
		if !have.Contains(ev.Type) && !slices.Contains(byType[ev.Type], ev.Resource) {
			byType[ev.Type] = append(byType[ev.Type], ev.Resource)
		}
	}
	missing := make([]MissingSubscription, 0, len(byType))
	for t, res := range byType {
		missing = append(missing, MissingSubscription{Type: t, Resources: res})
	}
	slices.SortFunc(missing, func(a, b MissingSubscription) int { return strings.Compare(a.Type, b.Type) })
	return missing, nil
}
