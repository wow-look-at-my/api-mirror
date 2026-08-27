package mirror

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Delivery is one received webhook, after signature verification.
type Delivery struct {
	ID      string
	Type    string
	Event   *Event
	Payload any
	Raw     []byte
	// Subject is what this delivery is a view of, and At is the moment that
	Subject string
	At      time.Time
}

// orderOf resolves a delivery's subject and clock from its event declaration.
//
// The clock must be a field the PROVIDER sets and that describes the SUBJECT.
// A user-settable timestamp fails both ways: it survives a rebase and it can be
// in the future, so a payload carrying one orders itself ahead of the truth.
// The spec chooses the field; the engine only refuses to guess when it is
// missing.
func orderOf(ev *Event, payload any) (string, time.Time, error) {
	subject, err := renderString(ev.Subject, map[string]any{"payload": payload})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("event %q subject: %w", ev.Type, err)
	}
	if subject == "" {
		return "", time.Time{}, fmt.Errorf("event %q: subject rendered empty", ev.Type)
	}
	if ev.Unordered {
		return subject, time.Time{}, nil
	}
	raw := lookupPath(payload, ev.Clock)
	if raw == nil {
		// The spec named a clock field and this payload does not carry it. That
		return subject, time.Time{}, fmt.Errorf("event %q: payload carries no %s", ev.Type, ev.Clock)
	}
	secs, err := toUnix(raw)
	if err != nil {
		return subject, time.Time{}, fmt.Errorf("event %q clock %s: %w", ev.Type, ev.Clock, err)
	}
	n, ok := secs.(int64)
	if !ok {
		return subject, time.Time{}, fmt.Errorf("event %q clock %s: empty", ev.Type, ev.Clock)
	}
	return subject, time.Unix(n, 0).UTC(), nil
}

// DeliveryDisposition is what happened to one delivery. It is reported back to
// the provider, so it lands in their delivery record.
type DeliveryDisposition string

const (
	DeliveryApplied     DeliveryDisposition = "applied"
	DeliverySuperseded  DeliveryDisposition = "superseded"
	DeliveryInvalidated DeliveryDisposition = "invalidated"
	DeliveryIgnored     DeliveryDisposition = "ignored"
	DeliveryFailed      DeliveryDisposition = "error"
)

// applyFunc applies one delivery to the store.
type applyFunc func(ctx context.Context, d *Delivery) (DeliveryDisposition, error)

// Reorderer sorts deliveries that land close together for the SAME subject and
// applies them oldest-first.
type Reorderer struct {
	window time.Duration
	apply  applyFunc

	mu      sync.Mutex
	batches map[string]*batch
	wg      sync.WaitGroup
	// now is a package var in disguise so a test can drive the clock.
	now func() time.Time
}

type batch struct {
	items []*Delivery
	timer *time.Timer
}

// NewReorderer returns a reorderer. A zero window applies on arrival and leaves
// ordering entirely to the watermark.
func NewReorderer(window time.Duration, apply applyFunc) *Reorderer {
	return &Reorderer{
		window:  window,
		apply:   apply,
		batches: make(map[string]*batch),
		now:     time.Now,
	}
}

// Submit hands one delivery to the reorderer.
//
// With a window it returns immediately and the delivery applies later, so the
// caller answers the provider before the write lands. That is deliberate: a
func (r *Reorderer) Submit(d *Delivery) {
	if r.window <= 0 {
		r.wg.Add(1)
		go r.run([]*Delivery{d})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.batches[d.Subject]
	if !ok {
		b = &batch{}
		r.batches[d.Subject] = b
		subject := d.Subject
		b.timer = time.AfterFunc(r.window, func() { r.close(subject) })
	}
	b.items = append(b.items, d)
}

func (r *Reorderer) close(subject string) {
	r.mu.Lock()
	b := r.batches[subject]
	delete(r.batches, subject)
	r.mu.Unlock()
	if b == nil || len(b.items) == 0 {
		return
	}
	r.wg.Add(1)
	r.run(b.items)
}

// run sorts one subject's batch by its payload clocks and applies it in order.
func (r *Reorderer) run(items []*Delivery) {
	defer r.wg.Done()
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	// The context is detached from any request: the caller has already been
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()
	for _, d := range items {
		if _, err := r.apply(ctx, d); err != nil {
			logf("delivery %s (%s): %v", d.ID, d.Type, err)
		}
	}
}

// applyTimeout bounds one batch's writes. It exists so a wedged store cannot
const applyTimeout = 2 * time.Minute

// Drain waits for held and in-flight deliveries, so a shutdown does not drop a
// delivery the provider will never send again.
func (r *Reorderer) Drain(timeout time.Duration) bool {
	r.mu.Lock()
	subjects := make([]string, 0, len(r.batches))
	for s := range r.batches {
		subjects = append(subjects, s)
	}
	r.mu.Unlock()
	for _, s := range subjects {
		r.mu.Lock()
		b := r.batches[s]
		r.mu.Unlock()
		if b != nil && b.timer != nil && b.timer.Stop() {
			r.close(s)
		}
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
