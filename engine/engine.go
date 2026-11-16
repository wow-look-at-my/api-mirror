// Package engine embeds an api-mirror engine in a host program. It is a thin
// wrapper: the standalone server builds, starts and shuts down through the
// same calls.
package engine

import (
	"context"
	"net/http"
	"time"

	"github.com/wow-look-at-my/api-mirror/internal/mirror"
)

// Options configures an embedded engine.
type Options struct {
	// DB is the path of the SQLite cache. A schema change nukes it on open.
	DB string
	// OnExchange receives every outbound request the engine sends. Optional.
	OnExchange func(Exchange)
	// OnRequest receives every inbound request on the handler's goroutine, before ServeHTTP returns. Optional.
	OnRequest func(Request)
}

// Engine serves one spec. It is the http.Handler the standalone server runs.
type Engine struct {
	m *mirror.Engine
}

// New builds an engine from spec source, such as an embedded file. The name
// labels load errors.
func New(ctx context.Context, name string, spec []byte, opts Options) (*Engine, error) {
	s, err := mirror.LoadSource(name, spec)
	if err != nil {
		return nil, err
	}
	return build(ctx, s, opts)
}

// NewFromFile builds an engine from the spec at path.
func NewFromFile(ctx context.Context, path string, opts Options) (*Engine, error) {
	s, err := mirror.Load(path)
	if err != nil {
		return nil, err
	}
	return build(ctx, s, opts)
}

func build(ctx context.Context, s *mirror.Spec, opts Options) (*Engine, error) {
	tel := mirror.NewTelemetry(s.Upstream.Rate)
	var out func(mirror.Exchange)
	if opts.OnExchange != nil {
		out = func(x mirror.Exchange) { opts.OnExchange(exchangeOf(x)) }
	}
	var in func(mirror.Request)
	if opts.OnRequest != nil {
		in = func(r mirror.Request) { opts.OnRequest(requestOf(r)) }
	}
	tel.Tap(out, in)
	m, err := mirror.Build(ctx, s, opts.DB, tel)
	if err != nil {
		return nil, err
	}
	return &Engine{m: m}, nil
}

// WithPrincipal names the caller of a request the host authenticated.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return mirror.WithHostPrincipal(ctx, principal)
}

// ServeHTTP answers a request exactly as the standalone server does.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.m.ServeHTTP(w, r) }

// Declares reports whether a spec route answers method on path, the escaped. URL path.
func (e *Engine) Declares(method, path string) bool { return e.m.Declares(method, path) }

// EventsPath is where the spec takes webhook deliveries, or empty without <events>.
func (e *Engine) EventsPath() string { return e.m.EventsPath() }

// Start runs the background jobs: credential announce, refresh and replay.
func (e *Engine) Start() { e.m.Start() }

// Drain stops the background jobs and waits up to timeout for in-flight fetches, deliveries and notifications.
func (e *Engine) Drain(timeout time.Duration) bool { return e.m.Drain(timeout) }

// Close releases the databases. Call it after Drain.
func (e *Engine) Close() error { return e.m.Close() }
