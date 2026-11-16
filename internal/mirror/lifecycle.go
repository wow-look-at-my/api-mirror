// lifecycle.go holds LoadSource.

package mirror

import (
	"context"
	"errors"
	"fmt"
)

// LoadSource builds and validates a spec from XML source. The name labels
// every error. A builder checks the SHAPE of the file, and validate checks
// what the shape MEANS.
func LoadSource(name string, raw []byte) (*Spec, error) {
	spec, err := ParseSpec(raw)
	if err != nil {
		return nil, fmt.Errorf("parse spec %q: %w", name, err)
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("validate spec %q: %w", name, err)
	}
	return spec, nil
}

// Build opens the store at dbPath and wires an engine, its ingest included.
// The standalone server and an embedding host both start here. On an error it
// releases what it already opened.
func Build(ctx context.Context, spec *Spec, dbPath string, tel *Telemetry) (*Engine, error) {
	store, err := Open(ctx, dbPath, spec)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(spec, store, tel)
	if err != nil {
		store.Close()
		return nil, err
	}
	if spec.Events != nil {
		ingest, err := NewIngest(spec, store, engine.vars)
		if err != nil {
			engine.Close()
			return nil, err
		}
		ingest.SetTelemetry(engine.tel)
		ingest.SetNotifier(engine.notify)
		engine.SetIngest(ingest)
	}
	return engine, nil
}

// Start runs the background half: the credential announce, the refresh sweep
// and the delivery replay. Drain stops them again.
func (e *Engine) Start() {
	e.up.creds.announce()
	e.refresh.Start()
	e.replay.Start()
}

// Close releases the subscription database and then the store. Call it after
// Drain, so no in-flight write meets a closed database.
func (e *Engine) Close() error {
	return errors.Join(e.notify.Close(), e.store.Close())
}

// Declares reports whether a route answers method on path, an escaped URL
// path. It applies a <rewrite> first and uses the matcher dispatch uses.
func (e *Engine) Declares(method, path string) bool {
	if p, ok := e.rewritePath(path); ok {
		path = p
	}
	for _, rt := range e.spec.Routes {
		if rt.Method != method {
			continue
		}
		if params, ok := matchPath(rt.Path, path); ok && rt.admits(params) {
			return true
		}
	}
	return false
}

// EventsPath is where the upstream posts deliveries, or empty without <events>.
func (e *Engine) EventsPath() string {
	if e.spec.Events == nil {
		return ""
	}
	return e.spec.Events.Path
}
