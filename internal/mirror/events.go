package mirror

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// maxDeliveryBytes caps one delivery. A body past the cap is refused rather
const maxDeliveryBytes = 8 << 20

// deliverTimeout bounds the writes of one delivery answered in the request.
const deliverTimeout = 30 * time.Second

// dispositionHeader reports what the mirror did with a delivery. The provider
const dispositionHeader = "X-Mirror-Disposition"

// watermarkRetention is how long a subject's applied moment is kept.
const watermarkRetention = 30 * 24 * time.Hour

// watermarkPruneInterval throttles the sweep. Pruning is housekeeping, so it
const watermarkPruneInterval = 10 * time.Minute

// Ingest is the webhook endpoint: the upstream telling the mirror what changed.
//
// A delivery is the ANSWER, not a hint to go and ask. The apply path writes the
// values the payload carries and fetches nothing. It also writes ONLY the
// fields the event names, so a partial view cannot blank known state.
type Ingest struct {
	events  *Events
	store   *Store
	reorder *Reorderer
	// secret is the resolved HMAC key. An empty one refuses every delivery: an
	secret []byte
	byType map[string]*Event
	window time.Duration
	now    func() time.Time
	// lastPrune stamps the last watermark sweep, as a Unix second.
	lastPrune atomic.Int64
	// tel puts every delivery on the chart. Nil-safe, so a test can skip it.
	tel      *Telemetry
	notifier *Notifier
	stats    deliveryStats
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
		// The endpoint refuses every delivery until the secret arrives. Say so
		logf("<events>: the secret resolved to nothing -- every delivery is refused until it is set")
	}
	i := &Ingest{
		events: spec.Events,
		store:  store,
		secret: []byte(secret),
		byType: make(map[string]*Event, len(spec.Events.List)),
		window: spec.Events.ReorderWindow,
		now:    time.Now,
	}
	for _, ev := range spec.Events.List {
		i.byType[ev.Type] = ev
	}
	i.reorder = NewReorderer(i.window, i.applyAndNotify)
	return i, nil
}

// Path is where the upstream posts deliveries.
func (i *Ingest) Path() string { return i.events.Path }

// Handler serves that path.
func (i *Ingest) Handler() http.Handler { return i }

// Reorderer is the window behind the handler. A shutdown waits on it, so it is
func (i *Ingest) Reorderer() *Reorderer { return i.reorder }

// Drain waits for held and in-flight deliveries at shutdown. A provider sends a
func (i *Ingest) Drain(timeout time.Duration) bool { return i.reorder.Drain(timeout) }

// ServeHTTP receives one delivery.
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
	ev, ok := i.byType[typ]
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
	subject, at, err := orderOf(ev, payload)
	if err != nil {
		logf("delivery %s: %v", typ, err)
		i.answer(w, DeliveryFailed)
		return
	}
	d := &Delivery{
		ID:      deliveryID(raw),
		Type:    typ,
		Event:   ev,
		Payload: payload,
		Raw:     raw,
		Subject: subject,
		At:      at,
	}

	if i.window > 0 {
		// The provider waits for this answer and gives up in single-digit
		i.reorder.Submit(d)
		i.reply(w, http.StatusAccepted, "accepted")
		return
	}
	// With no window there is nothing to wait for, so the answer carries the
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), deliverTimeout)
	defer cancel()
	disp, err := i.applyAndNotify(ctx, d)
	if err != nil {
		logf("delivery %s (%s): %v", d.ID, d.Type, err)
	}
	i.answer(w, disp)
}

// answer reports a disposition to the provider.
func (i *Ingest) answer(w http.ResponseWriter, d DeliveryDisposition) {
	i.reply(w, deliveryStatus(d), string(d))
}

func (i *Ingest) reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(dispositionHeader, body)
	w.WriteHeader(status)
	fmt.Fprintln(w, body)
}

// deliveryStatus maps a disposition to the status the provider records.
//
// Every non-error is 2xx, so a healthy hook stays enabled. The 200/202 split
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
// far their guess got, which is enough to forge a signature one byte at a time.
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

// deliveryID names one delivery in the log.
func deliveryID(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

// apply writes one delivery to the store. It is the whole ingest contract:
// order first, then write only the fields the event names.
func (i *Ingest) apply(ctx context.Context, d *Delivery) (DeliveryDisposition, error) {
	ev := d.Event
	res, ok := i.store.Resource(ev.Resource)
	if !ok {
		return DeliveryFailed, fmt.Errorf("event %q names unknown resource %q", ev.Type, ev.Resource)
	}
	key, err := ingestKey(res, d.Payload)
	if err != nil {
		return DeliveryFailed, fmt.Errorf("event %q: %w", ev.Type, err)
	}

	superseded := i.order(ctx, d)
	if superseded && !ev.AbsorbWhenSuperseded {
		return DeliverySuperseded, nil
	}

	if ev.Invalidate != nil {
		if _, err := i.store.Delete(ctx, res, key); err != nil {
			return DeliveryFailed, err
		}
		i.prune(ctx)
		return DeliveryInvalidated, nil
	}
	if err := i.merge(ctx, res, ev, key, d.Payload); err != nil {
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
// A failure APPLIES the delivery, loudly. A provider sends a delivery once, so
func (i *Ingest) order(ctx context.Context, d *Delivery) bool {
	if d.Event.Unordered {
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
// A missing component is an error. A partial key matches rows the delivery is
// not about, so a write under one lands on the wrong row.
func ingestKey(res *Resource, payload any) (map[string]string, error) {
	key := make(map[string]string, len(res.Keys))
	for _, k := range res.Keys {
		if k.From == "" {
			return nil, fmt.Errorf("resource %q: key %q declares no from, so no delivery can address it", res.Name, k.Name)
		}
		v := lookupPath(payload, k.From)
		if v == nil {
			return nil, fmt.Errorf("resource %q: the payload carries no %s for key %q", res.Name, k.From, k.Name)
		}
		s := foldKey(k, fmt.Sprintf("%v", v))
		if s == "" {
			return nil, fmt.Errorf("resource %q: key %q resolved empty", res.Name, k.Name)
		}
		key[k.Name] = s
	}
	return key, nil
}

// merge overlays the fields this event names onto the stored row.
//
// The write touches ONLY the named fields, and a named field the payload does
// not state leaves its column alone. That is what stops a partial view from
// blanking known state. AllowNull is the opt-in for a column where cleared and
// absent are different answers.
func (i *Ingest) merge(ctx context.Context, res *Resource, ev *Event, key map[string]string, payload any) error {
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
	return i.store.Put(ctx, res, merged, i.now())
}

// setValue reads one declared write out of the payload and coerces it to the
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
// It is best effort and throttled. A failed sweep costs disk, so it is logged
// and the delivery stands; failing the write over housekeeping would lose a
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
