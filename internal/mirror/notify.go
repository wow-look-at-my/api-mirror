package mirror

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// notifyDefaults back a declaration that leaves them out.
const (
	notifyDefaultTimeout = 10 * time.Second
	notifyDefaultRetries = 3
	notifyDefaultDisable = 20
	notifyRetryPause     = 2 * time.Second
)

// Notification is the body a subscriber receives.
type Notification struct {
	Mirror      string              `json:"mirror"`
	Delivery    string              `json:"delivery"`
	Type        string              `json:"type"`
	Resource    string              `json:"resource"`
	Subject     string              `json:"subject"`
	Disposition DeliveryDisposition `json:"disposition"`
	At          time.Time           `json:"at"`
}

// Notifier fans applied delivery out to every matching subscription, so a
// consumer stops racing this mirror's ingestion with its own webhooks.
type Notifier struct {
	spec   *Spec
	rule   *Notify
	subs   *Subscriptions
	client *http.Client
	tel    *Telemetry

	wg sync.WaitGroup

	mu        sync.Mutex
	sent      int
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
	return &Notifier{
		spec:   spec,
		rule:   rule,
		subs:   subs,
		client: observedClient(&http.Client{Timeout: notifyTimeout(rule)}, LaneNotify, tel),
		tel:    tel,
	}, nil
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
// mirror's ingestion: the delivery reaches them, they read the mirror,
// and they get the state that delivery was about to replace. Telling them
// AFTER the write lands removes the race instead of narrowing it.
//
// Delivery is detached from the request that triggered it: the provider is
// waiting on the ingest response, and making them wait on a subscriber's own
// slow endpoint turns consumer's outage into a lost delivery for everyone.
func (n *Notifier) Fan(ctx context.Context, d *Delivery, disp DeliveryDisposition) {
	if n == nil {
		return
	}
	subs, err := n.subs.Matching(ctx, d.Type)
	if err != nil {
		logf("notify: list subscriptions: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	note := Notification{
		Mirror:      n.spec.Name,
		Delivery:    d.ID,
		Type:        d.Type,
		Resource:    d.Event.Resource,
		Subject:     d.Subject,
		Disposition: disp,
		At:          time.Now().UTC(),
	}
	// Marshalled, never assembled: quote in a subject reshapes a splice.
	body, err := marshalJSON(note)
	if err != nil {
		logf("notify: render notification: %v", err)
		return
	}

	detached := context.WithoutCancel(ctx)
	for _, s := range subs {
		n.wg.Add(1)
		go func(s Subscription) {
			defer n.wg.Done()
			n.deliver(detached, s, body)
		}(s)
	}
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
// back at a moment nothing here can predict, and a growing delay means the
// notification after they return is the that waited longest.
func (n *Notifier) deliver(ctx context.Context, s Subscription, body []byte) {
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
		status, err := n.post(ctx, s, body)
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

func (n *Notifier) post(ctx context.Context, s Subscription, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
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
// disabled with the reason, so an operator sees a consumer that stopped being
// told rather than that silently never was.
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
	Failed        int       `json:"failed"`
	Disabled      int       `json:"disabled"`
	LastSent      time.Time `json:"last_sent,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	DB            string    `json:"db,omitempty"`
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
		Failed:    n.failed,
		Disabled:  n.disabled,
		LastSent:  n.lastSent,
		LastError: n.lastError,
		DB:        n.subs.Path(),
	}
	n.mu.Unlock()
	if count, err := n.subs.Count(ctx); err == nil {
		s.Subscriptions = count
	}
	return s
}
