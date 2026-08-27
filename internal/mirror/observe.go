package mirror

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Lane names the kind of traffic one exchange belongs to. It is the grain the
// dashboard groups by, so a lane the code can produce but the vocabulary cannot
// name is a hole in the chart.
type Lane string

const (
	LaneFetch       Lane = "fetch"       // a cached route bringing its own key up to date
	LaneProbe       Lane = "probe"       // the reveal layer proving one caller's access
	LaneRefresh     Lane = "refresh"     // the periodic background sweep
	LanePassthrough Lane = "passthrough" // a request the spec does not model, forwarded
	LaneReplay      Lane = "replay"      // asking the upstream to re-send a lost delivery
	LaneDelivery    Lane = "delivery"    // an inbound webhook, timed at the handler
	LaneNotify      Lane = "notify"      // an outbound subscriber notification
)

// Exchange is one completed request, inbound or outbound, as reported to the
// telemetry sink. Everything the mirror exchanges lands here: a request nobody
// can see is a request nobody can account for.
type Exchange struct {
	Lane     Lane
	Method   string
	URL      string
	Path     string
	Status   int
	Bytes    int
	Started  time.Time
	Duration time.Duration
	// Principal is who the exchange was made on behalf of, where that is known.
	Principal string
	// Detail carries the lane's own vocabulary: a passthrough reason, a delivery
	// disposition, an event type. It is display text, never a key.
	Detail string
	Err    error
}

// laneKey carries the lane and principal of the request being made into the
// transport, which is the only place that sees every outbound request.
type laneKey struct{}

type laneTag struct {
	lane      Lane
	principal string
	detail    string
}

func withLane(ctx context.Context, lane Lane, principal, detail string) context.Context {
	return context.WithValue(ctx, laneKey{}, laneTag{lane: lane, principal: principal, detail: detail})
}

func laneFrom(ctx context.Context) laneTag {
	t, _ := ctx.Value(laneKey{}).(laneTag)
	return t
}

// Observer is what a reporting client tells. Two calls rather than one because
// an exchange is reported when its body is done, and a rate-limit budget is a
// property of headers that arrive long before that.
type Observer interface {
	Observe(Exchange)
	ObserveHeaders(principal string, h http.Header)
}

// funcObserver adapts a plain callback, for a caller that wants the exchanges
// and has no use for the budget headers.
type funcObserver func(Exchange)

func (f funcObserver) Observe(e Exchange)               { f(e) }
func (funcObserver) ObserveHeaders(string, http.Header) {}

// observeTransport reports every request that passes through it.
//
// Observation lives in the TRANSPORT, not at the call sites, because call sites
// only cover the calls somebody remembered to instrument. A client built here
// cannot make an invisible request, whoever writes the next caller.
type observeTransport struct {
	base    http.RoundTripper
	lane    Lane
	obs     Observer
	timeNow func() time.Time
}

// observedClient derives a reporting client from one that is not. Timeout is
// carried over so the derived client keeps the deadline the original had.
func observedClient(base *http.Client, lane Lane, obs Observer) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	return &http.Client{
		Timeout:   base.Timeout,
		Transport: observing(base.Transport, lane, obs),
		// A redirect is its own request and reports on its own; the hop count is
		// the default because nothing here has a reason to differ.
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
	}
}

func observing(base http.RoundTripper, lane Lane, obs Observer) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if obs == nil {
		obs = funcObserver(func(Exchange) {})
	}
	return &observeTransport{base: base, lane: lane, obs: obs, timeNow: time.Now}
}

func (t *observeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := t.timeNow()
	tag := laneFrom(req.Context())
	lane := t.lane
	if tag.lane != "" {
		lane = tag.lane
	}
	base := Exchange{
		Lane:      lane,
		Method:    req.Method,
		URL:       req.URL.String(),
		Path:      req.URL.Path,
		Started:   started,
		Principal: tag.principal,
		Detail:    tag.detail,
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		base.Duration = t.timeNow().Sub(started)
		base.Err = err
		t.obs.Observe(base)
		return nil, err
	}

	base.Status = resp.StatusCode
	// The budget is read here, from the headers, because the exchange itself is
	// not reported until the body is done and a budget read that late describes
	// a window that may already have rolled over.
	t.obs.ObserveHeaders(tag.principal, resp.Header)

	// The exchange is reported when its body is done, not when the headers
	// arrive: a streamed answer costs what it costs until the last byte, and a
	// byte count taken before the read is always zero.
	resp.Body = &reportingBody{
		ReadCloser: resp.Body,
		emit: func(n int) {
			done := base
			done.Bytes = n
			done.Duration = t.timeNow().Sub(started)
			t.obs.Observe(done)
		},
	}
	return resp, nil
}

// reportingBody counts what a response actually cost and reports once, when the
// body is closed. Close is the one call every consumer of an http.Response
// makes, so it is the only place a report cannot be skipped.
type reportingBody struct {
	io.ReadCloser
	emit func(n int)
	n    int
	once sync.Once
}

func (b *reportingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += n
	return n, err
}

func (b *reportingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.emit(b.n) })
	return err
}

// pathOf reduces a URL to its path, for a report about a request that failed
// before it had a parsed URL to report.
func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}
