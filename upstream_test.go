package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// swapUpstreamClient points the one outbound client at a test's own, and puts
// the real one back. NewUpstreamer captures the package var, so the swap has to
// happen before the upstreamer is built.
func swapUpstreamClient(t *testing.T, c *http.Client) {
	t.Helper()
	saved := upstreamClient
	upstreamClient = c
	t.Cleanup(func() { upstreamClient = saved })
}

// upstreamSpec declares two static headers and one forwarded one, which is the
// whole outbound header story in a single fixture.
func upstreamSpec(base string) *Spec {
	return &Spec{
		Name: "up",
		Upstream: Upstream{
			Base: base,
			Headers: []Header{
				{Name: "Accept", Value: "application/vnd.example+json"},
				{Name: "X-Api-Version", Value: "{{ .var.version }}"},
			},
			Forward: []string{"Authorization"},
		},
	}
}

// callVars is the context the engine hands an upstream call: the spec's vars
// under `.var`, beside the environment. A test that flattened it would render
// `.var.x` to nothing and prove the opposite of what it claims.
var callVars = map[string]any{
	"env": map[string]string{},
	"var": map[string]any{"version": "2026-01-01", "blank": ""},
}

// newUpstreamer wires an upstreamer to a handler, and records every exchange it
// reports. The recorded exchanges are the assertion for the visibility rule: a
// request nobody can see is a request nobody can account for.
func newUpstreamer(t *testing.T, h http.HandlerFunc) (*Upstreamer, *httptest.Server, *[]Exchange) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	var seen []Exchange
	up, err := NewUpstreamer(upstreamSpec(srv.URL), callVars,
		func(e Exchange) { seen = append(seen, e) })
	require.NoError(t, err)
	return up, srv, &seen
}

func TestUpstreamCall_SendsStaticHeadersAndTheCallersOwn(t *testing.T) {
	var got http.Header
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"ok":true}`))
	})

	forward := http.Header{}
	forward.Set("Authorization", "Bearer caller-token")
	forward.Set("X-Trace", "not-forwarded")

	answer, err := up.Call(context.Background(), http.MethodGet, "/things/1", callVars, forward)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, answer.Status)
	assert.Equal(t, `{"ok":true}`, string(answer.Body))

	assert.Equal(t, "application/vnd.example+json", got.Get("Accept"), "a static header reaches the upstream")
	assert.Equal(t, "2026-01-01", got.Get("X-Api-Version"),
		"a static header is template source, rendered against the spec's vars")
	assert.Equal(t, "Bearer caller-token", got.Get("Authorization"),
		"the caller's own credential rides along; without it a probe proves the mirror's access, not theirs")
	assert.Empty(t, got.Get("X-Trace"),
		"only the headers the spec names are forwarded, so a caller cannot smuggle one upstream")
	assert.Equal(t, "identity", got.Get("Accept-Encoding"),
		"the mirror parses and rebuilds the body, so a compressed one would only have to be decoded here")
}

func TestUpstreamCall_MissingForwardedHeaderIsNotSentEmpty(t *testing.T) {
	var got http.Header
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	})
	_, err := up.Call(context.Background(), http.MethodGet, "/things/1", callVars, http.Header{})
	require.NoError(t, err)
	_, present := got["Authorization"]
	assert.False(t, present,
		"an absent credential must stay absent; sending an empty one asks the upstream a different question")
}

// A non-2xx is the upstream stating something it believes. Turning it into a Go
// error would throw away the status the route needs to decide whether to store.
func TestUpstreamCall_NonSuccessIsAnAnswerNotAnError(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		up, _, seen := newUpstreamer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"nope"}`))
		})
		answer, err := up.Call(context.Background(), http.MethodGet, "/things/1", callVars, http.Header{})
		require.NoErrorf(t, err, "%d is an answer; only a transport failure is an error", status)
		assert.Equal(t, status, answer.Status)
		assert.Equal(t, `{"message":"nope"}`, string(answer.Body), "the body of a refusal is relayed too")
		require.Len(t, *seen, 1)
		assert.Equal(t, status, (*seen)[0].Status, "the observer sees the status, so the exchange is accountable")
	}
}

func TestUpstreamCall_TransportFailureIsAnErrorAndIsStillReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // Nothing listens now, so the dial fails rather than the request.

	var seen []Exchange
	up, err := NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: base}}, map[string]any{},
		func(e Exchange) { seen = append(seen, e) })
	require.NoError(t, err)

	answer, err := up.Call(context.Background(), http.MethodGet, "/things/1", callVars, http.Header{})
	require.Error(t, err, "a request that never reached the upstream must not read as an answer")
	assert.Nil(t, answer)
	assert.Contains(t, err.Error(), "/things/1", "the message names the request that failed")

	require.Len(t, seen, 1, "a failed request still spends time and must still appear on the chart")
	assert.Error(t, seen[0].Err)
	assert.Zero(t, seen[0].Status, "there was no status, and inventing one would misreport the failure")
}

func TestUpstreamCall_CancelledContextFails(t *testing.T) {
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := up.Call(ctx, http.MethodGet, "/things/1", callVars, http.Header{})
	require.Error(t, err, "a caller that gave up must not have its request reported as answered")
}

func TestUpstreamCall_RejectsAnUnusableMethod(t *testing.T) {
	up, _, _ := newUpstreamer(t, func(http.ResponseWriter, *http.Request) {})
	_, err := up.Call(context.Background(), "BAD METHOD", "/x", callVars, http.Header{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build upstream request")
}

// A body past the cap is relayed and not stored. The mirror declines to hold
// what it cannot hold, out loud, rather than storing a truncated document that
// reads to every later consumer as a complete one.
func TestUpstreamCall_BodyPastTheCapIsFlaggedAsOverflow(t *testing.T) {
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", maxBodyBytes+64)))
	})
	answer, err := up.Call(context.Background(), http.MethodGet, "/big", callVars, http.Header{})
	require.NoError(t, err)
	assert.True(t, answer.Overflow, "an oversized body must be reported, never silently truncated into the cache")
	assert.Len(t, answer.Body, maxBodyBytes, "what is kept is exactly the cap, so the reader knows what it holds")
}

func TestUpstreamCall_BodyAtTheCapIsNotOverflow(t *testing.T) {
	up, _, _ := newUpstreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", maxBodyBytes)))
	})
	answer, err := up.Call(context.Background(), http.MethodGet, "/big", callVars, http.Header{})
	require.NoError(t, err)
	assert.False(t, answer.Overflow, "the cap is inclusive; a body exactly at it is complete and storable")
}

func TestUpstreamCall_SkipsAHeaderThatRendersToNothing(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	t.Cleanup(srv.Close)

	spec := &Spec{Name: "up", Upstream: Upstream{
		Base:    srv.URL,
		Headers: []Header{{Name: "X-Optional", Value: "{{ .var.blank }}"}},
	}}
	up, err := NewUpstreamer(spec, callVars, nil)
	require.NoError(t, err)

	_, err = up.Call(context.Background(), http.MethodGet, "/x", callVars, http.Header{})
	require.NoError(t, err)
	_, present := got["X-Optional"]
	assert.False(t, present, "a header whose value resolves to nothing is left off rather than sent blank")
}

// A header the spec cannot render is a spec mistake. The request must fail
// rather than go upstream missing the header the author declared.
func TestUpstreamCall_ReportsAHeaderThatWillNotRender(t *testing.T) {
	up, _, _ := newUpstreamer(t, func(http.ResponseWriter, *http.Request) {})

	up.headers = []Header{{Name: "X-Broken", Value: "{{ .oops"}}
	_, err := up.Call(context.Background(), http.MethodGet, "/x", callVars, http.Header{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "X-Broken", "the message names the header, which is what an author has to fix")

	up.headers = []Header{{Name: "{{ .oops", Value: "v"}}
	_, err = up.Call(context.Background(), http.MethodGet, "/x", callVars, http.Header{})
	require.Error(t, err, "a header NAME is template source too, and an unrenderable one is the same mistake")
	assert.Contains(t, err.Error(), "upstream header name")
}

func TestNewUpstreamer_ResolvesTheBaseOnce(t *testing.T) {
	hostVars := map[string]any{"var": map[string]any{"host": "https://api.example.com"}}
	up, err := NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: " {{ .var.host }}/ "}}, hostVars, nil)
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.com", up.base,
		"the base is trimmed of space and of a trailing slash, so a path concatenated onto it has one separator")

	// A template over a value the spec never set does not render to nothing; it
	// renders to something that is not a URL. Both shapes have to be refused
	// here, at boot, rather than at the first request.
	_, err = NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: "{{ .var.missing }}"}}, hostVars, nil)
	require.Error(t, err, "a base that resolves to nothing usable would send every request to a relative address")
	assert.Contains(t, err.Error(), "resolved to nothing")

	_, err = NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: "not-a-url"}}, hostVars, nil)
	require.Error(t, err, "a base with no scheme or host is not somewhere a request can be sent")

	_, err = NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: "{{ .var.host"}}, hostVars, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream base")
}

func TestNewUpstreamer_NilObserverBecomesANoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	up, err := NewUpstreamer(upstreamSpec(srv.URL), callVars, nil)
	require.NoError(t, err)
	require.NotNil(t, up.observe, "a caller that wants no observer still gets one, so Call never checks for nil")

	_, err = up.Call(context.Background(), http.MethodGet, "/x", callVars, http.Header{})
	require.NoError(t, err, "an unobserved upstreamer still works; the observer is optional, not load-bearing")
}

// The difference between a rate-limit refusal and an access refusal decides
// whether a denial is remembered. Caching a rate-limit answer locks a caller out
// of their own data for the whole deny window.
func TestRateLimited_TellsARefusalToWaitFromARefusalToRead(t *testing.T) {
	cases := []struct {
		name   string
		answer *Answer
		want   bool
	}{
		{name: "no answer at all", answer: nil, want: false},
		{
			name:   "a 429",
			answer: &Answer{Status: http.StatusTooManyRequests, Header: http.Header{}},
			want:   true,
		},
		{
			name: "a 403 with Retry-After",
			answer: &Answer{Status: http.StatusForbidden,
				Header: http.Header{"Retry-After": {"60"}}},
			want: true,
		},
		{
			name: "a 403 with the budget spent",
			answer: &Answer{Status: http.StatusForbidden,
				Header: http.Header{"X-Ratelimit-Remaining": {"0"}}},
			want: true,
		},
		{
			name: "a 403 with budget left",
			answer: &Answer{Status: http.StatusForbidden,
				Header: http.Header{"X-Ratelimit-Remaining": {"4999"}}},
			want: false,
		},
		{name: "a plain 403", answer: &Answer{Status: http.StatusForbidden, Header: http.Header{}}, want: false},
		{name: "a 404", answer: &Answer{Status: http.StatusNotFound, Header: http.Header{}}, want: false},
		{name: "a 200", answer: &Answer{Status: http.StatusOK, Header: http.Header{}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RateLimited(tc.answer),
				"%s decides whether a denial is cached; getting it wrong either locks a caller out or forgets a real verdict", tc.name)
		})
	}
}

func TestTransient_NamesWhatMustNeverBeStored(t *testing.T) {
	cases := []struct {
		name   string
		answer *Answer
		want   bool
	}{
		{name: "no answer at all", answer: nil, want: true},
		{name: "a 500", answer: &Answer{Status: http.StatusInternalServerError, Header: http.Header{}}, want: true},
		{name: "a 503", answer: &Answer{Status: http.StatusServiceUnavailable, Header: http.Header{}}, want: true},
		{name: "a 429", answer: &Answer{Status: http.StatusTooManyRequests, Header: http.Header{}}, want: true},
		{
			name: "a rate-limited 403",
			answer: &Answer{Status: http.StatusForbidden,
				Header: http.Header{"Retry-After": {"30"}}},
			want: true,
		},
		{name: "a plain 403", answer: &Answer{Status: http.StatusForbidden, Header: http.Header{}}, want: false},
		{name: "a 404", answer: &Answer{Status: http.StatusNotFound, Header: http.Header{}}, want: false},
		{name: "a 200", answer: &Answer{Status: http.StatusOK, Header: http.Header{}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Transient(tc.answer),
				"a transient failure stored is an outage remembered long after it ended")
		})
	}
}

func TestUpstreamCall_ObservesWhatTheExchangeCost(t *testing.T) {
	up, _, seen := newUpstreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	_, err := up.Call(context.Background(), http.MethodGet, "/things/1", callVars, http.Header{})
	require.NoError(t, err)

	require.Len(t, *seen, 1, "every outbound request is reported; an unreported one spends budget invisibly")
	e := (*seen)[0]
	assert.Equal(t, http.MethodGet, e.Method)
	assert.Contains(t, e.URL, "/things/1")
	assert.Equal(t, len(`{"ok":true}`), e.Bytes)
	assert.NoError(t, e.Err)
	assert.GreaterOrEqual(t, e.Duration, time.Duration(0))
}

// The client is a package var only so a test can point it somewhere. Proving the
// swap actually takes hold keeps a later test from silently talking to the real
// internet.
func TestUpstreamCall_UsesTheSwappedClient(t *testing.T) {
	swapUpstreamClient(t, &http.Client{Timeout: time.Second})
	up, err := NewUpstreamer(&Spec{Name: "up", Upstream: Upstream{Base: "https://api.example.com"}},
		map[string]any{}, nil)
	require.NoError(t, err)
	assert.Equal(t, time.Second, up.client.Timeout,
		"an upstreamer captures the package client at construction, so a swap must happen first")
}
