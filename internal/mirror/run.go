package mirror

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// drainTimeout bounds how long a shutdown waits for work already in flight.
const drainTimeout = 30 * time.Second

// Run parses the command line, serves until a signal, and drains. It returns
// the error the caller reports; nothing here calls os.Exit, so a test can drive
// it.
func Run() error {
	var (
		specPath = flag.String("spec", "mirror.xml", "path to the mirror spec")
		dbPath   = flag.String("db", "mirror.db", "path to the cache database")
		addr     = flag.String("listen", ":8080", "listen address")
		check    = flag.Bool("check", false, "load the spec, report what it derives, and exit")
	)
	flag.Parse()

	spec, err := Load(*specPath)
	if err != nil {
		return err
	}
	if *check {
		return report(spec, os.Stdout)
	}

	ctx := context.Background()
	store, err := Open(ctx, *dbPath, spec)
	if err != nil {
		return err
	}
	defer store.Close()

	engine, err := NewEngine(spec, store, nil)
	if err != nil {
		return err
	}
	if spec.Events != nil {
		ingest, err := NewIngest(spec, store, engine.vars)
		if err != nil {
			return err
		}
		engine.SetIngest(ingest)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	errs := make(chan error, 1)
	go func() {
		logger.Info("serving", "spec", spec.Name, "addr", *addr, "db", *dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-stop:
	}

	// Shut down in the order that keeps a write from reaching a closed
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logf("shutdown: %v", err)
	}
	if !engine.Drain(drainTimeout) {
		logf("gave up waiting for in-flight work after %s", drainTimeout)
	}
	return <-errs
}

// Drain waits for the work a shutdown must not interrupt: fetches that would
// otherwise write to a closed database, and deliveries the upstream will never
// send again.
func (e *Engine) Drain(timeout time.Duration) bool {
	ok := true
	if e.fresh != nil && !e.fresh.Drain(timeout) {
		ok = false
	}
	if e.ingest != nil && !e.ingest.Drain(timeout) {
		ok = false
	}
	return ok
}

// report prints what a spec derives, so an author can see the schema, the
// routes and the gates before running anything.
func report(spec *Spec, out *os.File) error {
	fmt.Fprintf(out, "mirror %s\n\n", spec.Name)
	fmt.Fprintf(out, "schema fingerprint: %s\n\n", spec.Fingerprint())
	fmt.Fprintln(out, spec.DDL())
	fmt.Fprintln(out, "routes:")
	for _, rt := range spec.Routes {
		kind := "one"
		if rt.List {
			kind = "list"
		}
		fmt.Fprintf(out, "\t%-6s %-40s -> %s (%s)\n", rt.Method, rt.Path, rt.Resource, kind)
	}
	if spec.Events != nil {
		fmt.Fprintf(out, "\nevents at %s:\n", spec.Events.Path)
		for _, ev := range spec.Events.List {
			fmt.Fprintf(out, "\t%-24s -> %s\n", ev.Type, ev.Resource)
		}
	}
	return nil
}
