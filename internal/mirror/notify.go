package mirror

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	notifyDefaultTimeout = 10 * time.Second
	notifyDefaultRetries = 3
	notifyDefaultDisable = 20
	notifyRetryPause     = 2 * time.Second
)

// Notifier fans applied delivery out to every matching subscription, so a
// consumer stops racing this mirror's ingestion with its own webhooks.
type Notifier struct {
	spec   *Spec
	rule   *Notify
	subs   *Subscriptions
	client *http.Client
	tel    *Telemetry
	// gate decides whether a subscriber's principal may hear about a delivery.
	// Nil lets through only the operator's own subscriptions.
	gate func(ctx context.Context, principal string, d *Delivery) bool
	// store and reveal gate each notification on the subscriber's access.
	store  *Store
	reveal *Revealer
	hdr    answerHeaders
	// imported is the startup import's result, or nil when none ran.
	imported *ImportReport

	wg sync.WaitGroup

	mu        sync.Mutex
	sent      int
	gated     int
	failed    int
	disabled  int
	lastSent  time.Time
	lastError string
}

// NewNotifier opens the subscription store and builds the fan-out.
func NewNotifier(spec *Spec, vars map[string]any, tel *Telemetry, cacheDB string) (*Notifier, error) {
	rule := spec.Notify
	path, err := renderString(rule.DB, vars)
	if err != nil {
		return nil, fmt.Errorf("<notify> db: %w", err)
	}
	subs, err := OpenSubscriptions(subscriptionsPath(path, cacheDB))
	if err != nil {
		return nil, err
	}
	n := &Notifier{
		spec:   spec,
		rule:   rule,
		subs:   subs,
		client: observedClient(&http.Client{Timeout: notifyTimeout(rule)}, LaneNotify, tel),
		tel:    tel,
		hdr:    headersFor(spec.HeaderPrefix),
	}
	if err := n.runImport(vars); err != nil {
		subs.Close()
		return nil, err
	}
	return n, nil
}

// runImport adopts the registry <notify import=> names. A declared value
// that renders empty names nothing, and says so once.
func (n *Notifier) runImport(vars map[string]any) error {
	if n.rule.Import == "" {
		return nil
	}
	src, err := renderString(n.rule.Import, vars)
	if err != nil {
		return fmt.Errorf("<notify> import: %w", err)
	}
	src = strings.TrimSpace(src)
	if src == "" {
		logger.Info("notify: <notify import=> renders empty, so no subscriptions are imported")
		return nil
	}
	report, err := importGSMSubscriptions(context.Background(), n.spec, n.subs, src)
	if err != nil {
		return err
	}
	n.imported = &report
	return nil
}

func notifyTimeout(rule *Notify) time.Duration {
	if rule.Timeout > 0 {
		return rule.Timeout
	}
	return notifyDefaultTimeout
}

// Subs exposes the subscription store to the HTTP surface that manages it.
func (n *Notifier) Subs() *Subscriptions {
	if n == nil {
		return nil
	}
	return n.subs
}

// Close releases the subscription database.
func (n *Notifier) Close() error {
	if n == nil {
		return nil
	}
	return n.subs.Close()
}

// Fan sends notification to every live subscription that wants this event.
//
// A consumer who also receives the upstream's webhooks otherwise races the
// mirror's ingestion. The delivery reaches them, they read the mirror, and
// they get the state that delivery was about to replace. Telling them
// AFTER the write lands removes the race instead of narrowing it.
//
// Delivery is detached from the request that triggered it. The provider is
// waiting on the ingest response. Making them wait on a subscriber's own
// slow endpoint turns consumer's outage into a lost delivery for everyone.
func (n *Notifier) Fan(ctx context.Context, d *Delivery, disp DeliveryDisposition) {
	if n == nil {
		return
	}
	matching, err := n.subs.Matching(ctx, d.Type)
	if err != nil {
		logf("notify: list subscriptions: %v", err)
		return
	}
	subs := matching[:0]
	for _, s := range matching {
		if s.Principal == operatorPrincipal || (n.gate != nil && n.gate(ctx, s.Principal, d)) {
			subs = append(subs, s)
			continue
		}
		n.mu.Lock()
		n.gated++
		n.mu.Unlock()
	}
	if len(subs) == 0 {
		return
	}
	note, err := n.body(d, disp)
	if err != nil {
		logf("notify: render notification for %s: %v", d.ID, err)
		return
	}
	scope, err := n.scopeOf(d)
	if err != nil {
		logf("notify: render scope for %s: %v", d.ID, err)
		return
	}

	detached := context.WithoutCancel(ctx)
	for _, s := range subs {
		if len(s.Scopes) > 0 && !scopeAdmits(s.Scopes, scope) {
			continue
		}
		if !n.visible(ctx, s.Principal, d) {
			continue
		}
		n.wg.Add(1)
		go func(s Subscription) {
			defer n.wg.Done()
			n.deliver(detached, s, d, note)
		}(s)
	}
}

// body builds the notification fields every subscriber shares. The spec's
// <field> declarations add values from the delivery.
func (n *Notifier) body(d *Delivery, disp DeliveryDisposition) (map[string]any, error) {
	now := time.Now().UTC()
	note := map[string]any{
		"mirror":          n.spec.Name,
		"delivery":        d.ID,
		"mirror_delivery": d.ID,
		"type":            d.Type,
		"event":           d.Type,
		"disposition":     disp,
		"at":              now,
		"ingested_at":     now.Format(time.RFC3339Nano),
	}
	if d.UpstreamID != "" {
		note["upstream_delivery"] = d.UpstreamID
	}
	ctx := map[string]any{
		"payload":  d.Payload,
		"delivery": map[string]any{"id": d.ID, "upstream_id": d.UpstreamID, "type": d.Type},
	}
	for _, f := range n.rule.Fields {
		var v any
		if f.Expr != "" {
			s, err := renderString(f.Expr, ctx)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", f.Field, err)
			}
			if s != "" {
				v = s
			}
		} else {
			v = lookupPath(d.Payload, f.From)
		}
		// An absent value is left out, never written as an empty one.
		if v != nil {
			note[f.Field] = v
		}
	}
	return note, nil
}

func (n *Notifier) scopeOf(d *Delivery) (string, error) {
	if n.rule.Scope == "" {
		return "", nil
	}
	return renderString(n.rule.Scope, map[string]any{"payload": d.Payload})
}

// visible reports whether a subscriber may hear about this delivery. With
// a <reveal> declared, the answer is the subscriber's stored proof for the
// named row, and anything that cannot be established reads as no.
func (n *Notifier) visible(ctx context.Context, principal string, d *Delivery) bool {
	rule := n.rule.Reveal
	if rule == nil {
		return true
	}
	if n.store == nil || n.reveal == nil {
		logf("notify: <reveal> is declared but the notifier has no store, so nothing is sent")
		return false
	}
	res, ok := n.store.Resource(rule.Resource)
	if !ok {
		return false
	}
	key := make(map[string]string, len(rule.Keys))
	for _, k := range rule.Keys {
		var v string
		if k.Expr != "" {
			s, err := renderString(k.Expr, map[string]any{"payload": d.Payload})
			if err != nil {
				return false
			}
			v = s
		} else {
			v = scalarString(lookupPath(d.Payload, k.From))
		}
		if v == "" {
			// The delivery does not name the row, so no one can be shown it.
			return false
		}
		key[k.Field] = foldFor(res, k.Field, v)
	}
	ok, err := n.reveal.AllowStored(ctx, principal, res, key)
	if err != nil {
		logf("notify: reveal for %s: %v -- not sent", principal, err)
		return false
	}
	return ok
}

// scopeAdmits reports whether a filter names the scope, or an ancestor of it
// at a "/" boundary. A filter "acme" thus admits "acme/widget".
func scopeAdmits(filters []string, scope string) bool {
	for _, f := range filters {
		if strings.EqualFold(f, scope) {
			return true
		}
		if f != "" && len(scope) > len(f) && scope[len(f)] == '/' && strings.EqualFold(scope[:len(f)], f) {
			return true
		}
	}
	return false
}

// Drain waits for notifications already in flight, so a shutdown does not drop
// that has been promised.
func (n *Notifier) Drain(timeout time.Duration) bool {
	if n == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// deliver posts notification, retrying a failure at a fixed cadence.
//
// The cadence is fixed rather than backing off. A subscriber that is down comes
// back at a moment nothing here can predict. A growing delay means the
// notification after they return is the that waited longest.
func (n *Notifier) deliver(ctx context.Context, s Subscription, d *Delivery, shared map[string]any) {
	note := make(map[string]any, len(shared)+2)
	for k, v := range shared {
		note[k] = v
	}
	note["subscription_id"] = s.ID
	retries := n.rule.Retries
	if retries <= 0 {
		retries = notifyDefaultRetries
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(notifyRetryPause):
			}
		}
		note["sent_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		// Marshalled, never assembled: a quote in a payload value reshapes a splice.
		body, err := marshalJSON(note)
		if err != nil {
			logf("notify: render notification: %v", err)
			return
		}
		status, err := n.post(ctx, s, d, body)
		if err == nil && status >= 200 && status < 300 {
			n.recordSent(ctx, s)
			return
		}
		lastErr = err
		if err == nil {
			lastErr = fmt.Errorf("subscriber answered %d", status)
		}
	}
	n.recordFailure(ctx, s, lastErr)
}

func (n *Notifier) post(ctx context.Context, s Subscription, d *Delivery, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cmp.Or(n.rule.EventHeader, n.hdr.Event), d.Type)
	req.Header.Set(cmp.Or(n.rule.DeliveryHeader, n.hdr.Delivery), d.ID)
	header := n.rule.SignatureHeader
	if header == "" {
		header = "X-Hub-Signature-256"
	}
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write(body)
	req.Header.Set(header, "sha256="+hex.EncodeToString(mac.Sum(nil)))

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Read and dropped so the transport reports and the connection is reusable.
	_, _, _ = readCapped(resp.Body)
	return resp.StatusCode, nil
}

func (n *Notifier) recordSent(ctx context.Context, s Subscription) {
	if err := n.subs.MarkDelivered(ctx, s.ID); err != nil {
		logf("notify: mark delivered %s: %v", s.ID, err)
	}
	n.mu.Lock()
	n.sent++
	n.lastSent = time.Now()
	n.mu.Unlock()
}

// recordFailure counts a failure and parks a subscription that keeps failing.
//
// Parking is loud, not quiet: the subscription stays in the store, marked
// disabled with the reason. An operator can tell a consumer the mirror gave up
// on from one it never reached.
func (n *Notifier) recordFailure(ctx context.Context, s Subscription, cause error) {
	limit := n.rule.DisableAfter
	if limit <= 0 {
		limit = notifyDefaultDisable
	}
	failures, err := n.subs.MarkFailed(ctx, s.ID, cause.Error(), limit)
	if err != nil {
		logf("notify: mark failed %s: %v", s.ID, err)
	}
	logf("notify: %s failed (%d consecutive): %v", s.URL, failures, cause)

	n.mu.Lock()
	n.failed++
	n.lastError = cause.Error()
	if failures >= limit {
		n.disabled++
	}
	n.mu.Unlock()
}

// NotifyStats is what the fan-out reports to the dashboard.
type NotifyStats struct {
	Enabled       bool      `json:"enabled"`
	Subscriptions int       `json:"subscriptions"`
	Sent          int       `json:"sent"`
	Gated         int       `json:"gated"`
	Failed        int       `json:"failed"`
	Disabled      int       `json:"disabled"`
	LastSent      time.Time `json:"last_sent,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	DB            string    `json:"db,omitempty"`
	// Import is what the startup import adopted, and which rows it could not.
	Import *ImportReport `json:"import,omitempty"`
}

// Stats reports the fan-out's own state.
func (n *Notifier) Stats(ctx context.Context) NotifyStats {
	if n == nil {
		return NotifyStats{}
	}
	n.mu.Lock()
	s := NotifyStats{
		Enabled:   true,
		Sent:      n.sent,
		Gated:     n.gated,
		Failed:    n.failed,
		Disabled:  n.disabled,
		LastSent:  n.lastSent,
		LastError: n.lastError,
		DB:        n.subs.Path(),
		Import:    n.imported,
	}
	n.mu.Unlock()
	if count, err := n.subs.Count(ctx); err == nil {
		s.Subscriptions = count
	}
	return s
}
