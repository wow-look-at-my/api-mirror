// events.go holds maxDeliveryBytes.

package mirror

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// maxDeliveryBytes caps delivery.
const maxDeliveryBytes = 8 << 20

// deliverTimeout bounds the writes of delivery answered in the request.
const deliverTimeout = 30 * time.Second

// watermarkRetention is how long a subject's applied moment is kept.
const watermarkRetention = 30 * 24 * time.Hour

// watermarkPruneInterval throttles the sweep. Pruning is housekeeping, so it
const watermarkPruneInterval = 10 * time.Minute

// Ingest is the webhook endpoint: the upstream telling the mirror what changed.
type Ingest struct {
	events  *Events
	store   *Store
	reorder *Reorderer
	// secret is the resolved HMAC key. An empty refuses every delivery.
	secret []byte
	byType map[string][]*Event
	window time.Duration
	now    func() time.Time
	// lastPrune stamps the last watermark sweep, as a Unix.
	lastPrune atomic.Int64
	// tel puts every delivery on the chart. Nil-safe, so a test can skip it.
	tel        *Telemetry
	notifier   *Notifier
	stats      deliveryStats
	log        deliveryLog
	ordering   orderingStats
	dispHeader string
}

// NewIngest builds the ingest endpoint a spec declares. vars is the spec's
// resolved vars, which the secret sees as `.var` beside the environment.
func NewIngest(spec *Spec, store *Store, vars map[string]any) (*Ingest, error) {
	if spec.Events == nil {
		return nil, fmt.Errorf("mirror %q declares no <events>", spec.Name)
	}
	secret, err := renderString(spec.Events.Secret, map[string]any{"env": envMap(), "var": vars})
	if err != nil {
		return nil, fmt.Errorf("<events> secret: %w", err)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		// The endpoint refuses every delivery until the secret arrives.
		logf("<events>: the secret resolved to nothing -- every delivery is refused until it is set")
	}
	i := &Ingest{
		events: spec.Events,
		store:  store,
		secret: []byte(secret),
		byType: make(map[string][]*Event, len(spec.Events.List)),
		window: spec.Events.ReorderWindow,
		// A provider records this header, so an operator reads what each delivery did.
		dispHeader: headersFor(spec.HeaderPrefix).Disposition,
		now:        time.Now,
	}
	// A type fans out to every event declared for it, in spec order. A push
	// states a branch tip AND makes the repo's file-derived answers wrong.
	for _, ev := range spec.Events.List {
		i.byType[ev.Type] = append(i.byType[ev.Type], ev)
	}
	i.reorder = NewReorderer(i.window, i.applyAndRecord)
	i.reorder.onBatch = i.ordering.batch
	return i, nil
}

// Path is where the upstream posts deliveries.
func (i *Ingest) Path() string { return i.events.Path }

// Handler serves that path.
func (i *Ingest) Handler() http.Handler { return i }

// Reorderer is the window behind the handler. A shutdown waits on it.
func (i *Ingest) Reorderer() *Reorderer { return i.reorder }

// Drain waits for held and in-flight deliveries at shutdown.
func (i *Ingest) Drain(timeout time.Duration) bool { return i.reorder.Drain(timeout) }

// ServeHTTP receives delivery.
//
// Every branch fails closed. A delivery whose authenticity the mirror cannot
// establish is refused, never treated as harmless.
func (i *Ingest) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(i.secret) == 0 {
		logf("delivery refused: the <events> secret is unset")
		http.Error(w, "ingest is not configured", http.StatusServiceUnavailable)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDeliveryBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !verifyDelivery(i.secret, r.Header.Get(i.events.SignatureHeader), raw) {
		logf("delivery refused: the signature does not verify")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	typ := r.Header.Get(i.events.TypeHeader)
	if typ == "" {
		http.Error(w, "the delivery states no event type", http.StatusBadRequest)
		return
	}
	evs, ok := i.byType[typ]
	if !ok {
		// A type the spec does not model is a real answer, not a failure. A 4xx
		i.answer(w, DeliveryIgnored)
		return
	}
	payload, err := decodeJSON(raw)
	if err != nil {
		http.Error(w, "the body is not JSON", http.StatusBadRequest)
		return
	}
	// Handlers order apart, each on its own subject. A shared subject makes the watermark refuse the rest.
	id := deliveryID(raw)
	deliveries := make([]*Delivery, 0, len(evs))
	for _, ev := range evs {
		ok, err := handles(ev, payload)
		if err != nil {
			logf("delivery %s (%s -> %s): %v", id, typ, ev.Resource, err)
			i.answer(w, DeliveryFailed)
			return
		}
		if !ok {
			continue
		}
		subject, at, err := orderOf(ev, payload)
		if err != nil {
			logf("delivery %s (%s -> %s): %v", id, typ, ev.Resource, err)
			i.answer(w, DeliveryFailed)
			return
		}
		deliveries = append(deliveries, &Delivery{
			ID:         id,
			Type:       typ,
			Event:      ev,
			Payload:    payload,
			Raw:        raw,
			Subject:    subject,
			At:         at,
			Received:   i.now(),
			UpstreamID: upstreamID(r, i.events.IDHeader),
		})
	}
	if len(deliveries) == 0 {
		// Every event for this type declined it through when=.
		i.answer(w, DeliveryIgnored)
		return
	}
	group := &deliveryGroup{pending: len(deliveries)}
	for _, d := range deliveries {
		d.group = group
	}
	if i.window > 0 {
		// The provider waits for this answer and gives up in single-digit
		for _, d := range deliveries {
			i.reorder.Submit(d)
		}
		i.reply(w, http.StatusAccepted, string(DeliveryHeld))
		return
	}
	// With no window there is nothing to wait for.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deliverTimeout)
	defer cancel()
	disp := DeliveryIgnored
	for _, d := range deliveries {
		one, err := i.applyAndNotify(ctx, d)
		if err != nil {
			logf("delivery %s (%s -> %s): %v", d.ID, d.Type, d.Event.Resource, err)
		}
		disp = foldDisposition(disp, one)
	}
	i.answer(w, disp)
}

// handles reports whether an event takes this delivery: its action is listed,
// and its when predicate renders truthy.
func handles(ev *Event, payload any) (bool, error) {
	if len(ev.Actions) > 0 && !containsString(ev.Actions, fmt.Sprint(lookupPath(payload, "action"))) {
		return false, nil
	}
	if ev.When == "" {
		return true, nil
	}
	out, err := renderString(ev.When, map[string]any{"payload": payload})
	if err != nil {
		return false, fmt.Errorf("event %q when: %w", ev.Type, err)
	}
	return isTruthy(out), nil
}

// foldDisposition answers a fanned-out delivery with its worst outcome, so a
// failed handler is never hidden behind a sibling that applied.
func foldDisposition(a, b DeliveryDisposition) DeliveryDisposition {
	if dispositionRank(b) > dispositionRank(a) {
		return b
	}
	return a
}

func dispositionRank(d DeliveryDisposition) int {
	switch d {
	case DeliveryFailed:
		return 4
	case DeliveryApplied:
		return 3
	case DeliveryInvalidated:
		return 2
	case DeliverySuperseded:
		return 1
	default:
		return 0
	}
}

// storedAgrees reports whether the stored row matches what the delivery
// states for one field. No stored row agrees, because nothing is there
// to contradict. A payload that states no value also agrees: silence is
// not a contradiction.
func (i *Ingest) storedAgrees(ctx context.Context, res *Resource, key map[string]string, inv *Invalidate, payload any) (bool, error) {
	stated := scalarString(lookupPath(payload, inv.Differs))
	if stated == "" {
		return true, nil
	}
	row, err := i.store.Get(ctx, res, key)
	if err != nil {
		return false, err
	}
	if row == nil {
		return true, nil
	}
	return fmt.Sprint(row[inv.Field]) == stated, nil
}

func upstreamID(r *http.Request, header string) string {
	if header == "" {
		return ""
	}
	return r.Header.Get(header)
}

// answer reports a disposition to the provider.
func (i *Ingest) answer(w http.ResponseWriter, d DeliveryDisposition) {
	i.reply(w, deliveryStatus(d), string(d))
}

func (i *Ingest) reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(i.dispHeader, body)
	w.WriteHeader(status)
	fmt.Fprintln(w, body)
}

// deliveryStatus maps a disposition to the status the provider records.
//
// Every non-error is 2xx, so a healthy hook stays enabled.
func deliveryStatus(d DeliveryDisposition) int {
	switch d {
	case DeliveryApplied:
		return http.StatusOK
	case DeliveryFailed:
		return http.StatusInternalServerError
	default:
		return http.StatusAccepted
	}
}

// verifyDelivery checks the HMAC over the RAW body.
//
// The comparison is constant time. A byte-by-byte comparison tells a sender how
// far their guess got, which is enough to forge a signature byte at a time.
func verifyDelivery(secret []byte, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

// deliveryID names delivery in the log.
func deliveryID(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

// apply writes delivery to the store. It is the whole ingest contract:
// order, then write only the fields the event names.
func (i *Ingest) apply(ctx context.Context, d *Delivery) (DeliveryDisposition, error) {
	ev := d.Event
	res, ok := i.store.Resource(ev.Resource)
	if !ok {
		return DeliveryFailed, fmt.Errorf("event %q names unknown resource %q", ev.Type, ev.Resource)
	}
	key, sent, err := ingestKey(res, ev, d.Payload)
	if err != nil {
		return DeliveryFailed, fmt.Errorf("event %q: %w", ev.Type, err)
	}

	superseded := i.order(ctx, d)
	if superseded && !ev.AbsorbWhenSuperseded {
		return DeliverySuperseded, nil
	}

	if ev.Invalidate != nil {
		if ev.Invalidate.Field != "" {
			agrees, err := i.storedAgrees(ctx, res, key, ev.Invalidate, d.Payload)
			if err != nil {
				return DeliveryFailed, err
			}
			if agrees {
				return DeliveryIgnored, nil
			}
		}
		del := i.store.Delete
		if ev.Invalidate.Every || ev.Invalidate.All {
			del = func(ctx context.Context, res *Resource, _ map[string]string) (int64, error) {
				return i.store.DeleteAll(ctx, res)
			}
		}
		if _, err := del(ctx, res, key); err != nil {
			return DeliveryFailed, err
		}
		i.forgetFreshness(ctx, res, key, ev.Invalidate.Every)
		i.prune(ctx)
		return DeliveryInvalidated, nil
	}
	write := i.merge
	if ev.Document != "" {
		write = i.putDocument
	}
	err = write(ctx, res, ev, key, sent, d.Payload)
	if errors.Is(err, errStoredIsNewer) {
		// A fetch already stored a later view than this delivery carries.
		return DeliverySuperseded, nil
	}
	if err != nil {
		return DeliveryFailed, err
	}
	i.prune(ctx)
	if superseded {
		// The fields still landed, because the event declares them immutable.
		return DeliverySuperseded, nil
	}
	return DeliveryApplied, nil
}

// order asks the watermark whether this view postdates what is already applied,
// and reports whether the watermark refused it.
//
// A failure APPLIES the delivery, loudly. A provider sends a delivery.
func (i *Ingest) order(ctx context.Context, d *Delivery) bool {
	if d.Event.Unordered || d.At.IsZero() {
		return false
	}
	applied, err := i.store.ApplyWatermark(ctx, d.Subject, d.At)
	if err != nil {
		logf("watermark %q: %v -- applying delivery %s unordered", d.Subject, err, d.ID)
		return false
	}
	return !applied
}

// ingestKey resolves every key component of a resource from the delivery
// payload.
//
// A write needs the whole key: a partial matches rows the delivery is not
// about, so a write under it lands on the wrong row. An invalidate is the case
// where a partial key is the right answer -- a payload names a commit, not page
// of a paginated answer. And there it deletes every row beneath it.
//
// sent holds each folded key's spelling as the payload states it, by the
// engine column that keeps it.
func ingestKey(res *Resource, ev *Event, payload any) (key, sent map[string]string, err error) {
	key = make(map[string]string, len(res.Keys))
	sent = map[string]string{}
	for _, k := range res.Keys {
		v, from, err := keyValue(ev, k, payload)
		if err != nil {
			return nil, nil, fmt.Errorf("resource %q: key %q: %w", res.Name, k.Name, err)
		}
		if from == "" {
			if ev.Invalidate != nil {
				continue
			}
			return nil, nil, fmt.Errorf("resource %q: this event says nowhere the delivery carries key %q", res.Name, k.Name)
		}
		if v == nil {
			return nil, nil, fmt.Errorf("resource %q: the payload carries no %s for key %q", res.Name, from, k.Name)
		}
		raw := fmt.Sprintf("%v", v)
		s := foldKey(k, raw)
		if s == "" {
			return nil, nil, fmt.Errorf("resource %q: key %q resolved empty", res.Name, k.Name)
		}
		key[k.Name] = s
		if c := k.sentColumn(); c != "" {
			sent[c] = raw
		}
	}
	return key, sent, nil
}

// keyValue reads key component out of a delivery, from the event's own
// <key> or from the <set> that writes that column. It also reports which of
// them it read, so a failure names a path the spec contains.
//
// The resource's from= is deliberately not consulted; validateEventKeys says
// why, and refuses an event that names neither.
func keyValue(ev *Event, k Key, payload any) (any, string, error) {
	for _, group := range [][]Set{ev.Keys, ev.Sets} {
		for _, st := range group {
			if st.Field != k.Name {
				continue
			}
			v, err := setValue(st, FieldText, payload)
			if err != nil {
				return nil, "", err
			}
			if st.Expr != "" {
				return v, "expr", nil
			}
			return v, st.From, nil
		}
	}
	return nil, "", nil
}

// merge overlays the fields this event names onto the stored row.
//
// The write touches ONLY the named fields, and a named field the payload does
// not state leaves its column alone. That is what stops a partial view from
// blanking known state. AllowNull is the opt-in for a column where cleared and
// absent are different answers.
func (i *Ingest) merge(ctx context.Context, res *Resource, ev *Event, key, sent map[string]string, payload any) error {
	row, err := i.store.Get(ctx, res, key)
	if err != nil {
		return err
	}
	merged := make(Row, len(res.Keys)+len(res.Fields))
	for column, v := range row {
		merged[column] = v
	}
	for name, v := range key {
		merged[name] = v
	}
	for column, v := range sent {
		merged[column] = v
	}
	for _, st := range ev.Sets {
		v, err := setValue(st, fieldTypeOf(res, st.Field), payload)
		if err != nil {
			return fmt.Errorf("event %q set %q: %w", ev.Type, st.Field, err)
		}
		if v == nil && !st.AllowNull {
			continue
		}
		merged[st.Field] = v
	}
	if err := i.store.Put(ctx, res, merged, i.now()); err != nil {
		return err
	}
	return i.recordDelivered(ctx, res, ev, key)
}

// setValue reads declared write out of the payload and coerces it to the
// column's type.
//
// A path that finds nothing yields nil, and the caller decides whether to write
// it. An expr always states a value, because a template renders a string.
func setValue(st Set, t FieldType, payload any) (any, error) {
	var raw any
	if st.Expr != "" {
		s, err := renderString(st.Expr, map[string]any{"payload": payload})
		if err != nil {
			return nil, err
		}
		raw = s
	} else {
		raw = lookupPath(payload, st.From)
	}
	return coerce(t, raw)
}

// fieldTypeOf reports a column's declared type. A key column is text; validate
// already refuses a set naming anything else.
func fieldTypeOf(res *Resource, name string) FieldType {
	for _, f := range res.Fields {
		if f.Name == name {
			return f.Type
		}
	}
	return FieldText
}

// prune sweeps watermarks nothing restates any more.
//
// It is best effort and throttled. A failed sweep costs disk. It is logged
// and the delivery stands. Failing the write over housekeeping would lose a
// delivery to save a row.
func (i *Ingest) prune(ctx context.Context) {
	now := i.now()
	last := i.lastPrune.Load()
	if now.Unix()-last < int64(watermarkPruneInterval.Seconds()) {
		return
	}
	if !i.lastPrune.CompareAndSwap(last, now.Unix()) {
		return
	}
	if _, err := i.store.PruneWatermarks(ctx, now.Add(-watermarkRetention)); err != nil {
		logf("prune watermarks: %v", err)
	}
}
