package mirror

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	defer engine.notify.Close()
	if spec.Events != nil {
		ingest, err := NewIngest(spec, store, engine.vars)
		if err != nil {
			return err
		}
		ingest.SetTelemetry(engine.tel)
		ingest.SetNotifier(engine.notify)
		engine.SetIngest(ingest)
	}
	engine.refresh.Start()
	engine.replay.Start()

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
		// The dashboard is on, and the token that opens it was minted for this
		// process. Printing the URL is not a convenience: a token nobody was
		// told is a dashboard nobody can open.
		if engine.admin.Minted() {
			logger.Info("dashboard", "url", engine.admin.URL(*addr), "token", "minted for this process")
		} else {
			logger.Info("dashboard", "url", engine.admin.URL(*addr))
		}
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
// otherwise write to a closed database, deliveries the upstream will never send
// again, and notifications a subscriber has already been promised.
//
// The background jobs are stopped FIRST. A sweep that starts a fetch while the
// drain is waiting is a fetch the drain never agreed to wait for.
func (e *Engine) Drain(timeout time.Duration) bool {
	e.refresh.Stop()
	e.replay.Stop()
	ok := true
	if e.fresh != nil && !e.fresh.Drain(timeout) {
		ok = false
	}
	if e.ingest != nil && !e.ingest.Drain(timeout) {
		ok = false
	}
	if !e.notify.Drain(timeout) {
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
	fmt.Fprintf(out, "\ndashboard at %s/\n", spec.Dashboard.Path)
	reportOps(spec, out)
	return nil
}

// reportOps prints the operational half, saying plainly which parts a spec
// leaves out. A mirror with no replay and no refresh is a valid mirror; one
// whose author did not realise those were choices is not.
func reportOps(spec *Spec, out *os.File) {
	say := func(name string, on bool, detail string) {
		state := "not declared"
		if on {
			state = detail
		}
		fmt.Fprintf(out, "\t%-12s %s\n", name, state)
	}
	say("refresh", spec.Refresh != nil, describeRefresh(spec.Refresh))
	say("replay", spec.Replay != nil, describeReplay(spec.Replay))
	say("notify", spec.Notify != nil, describeNotify(spec.Notify))
	say("cors", spec.CORS != nil, describeCORS(spec.CORS))
	say("health", spec.Health != nil, describeHealth(spec.Health))
	say("ratelimit", spec.Upstream.Rate.declared(), spec.Upstream.Rate.Remaining+" / "+spec.Upstream.Rate.Reset)
	say("debounce", spec.Upstream.Debounce > 0, spec.Upstream.Debounce.String())
}

func describeRefresh(r *Refresh) string {
	if r == nil {
		return ""
	}
	if len(r.Kinds) == 0 {
		return "every " + r.Interval.String() + ", every routed resource"
	}
	return "every " + r.Interval.String() + ", " + strings.Join(r.Kinds, " ")
}

func describeReplay(r *Replay) string {
	if r == nil {
		return ""
	}
	return "every " + r.Interval.String() + " from " + r.List
}

func describeNotify(n *Notify) string {
	if n == nil {
		return ""
	}
	return n.Path + " -> " + n.DB
}

func describeCORS(c *CORS) string {
	if c == nil {
		return ""
	}
	return strings.Join(c.Origins, " ")
}

func describeHealth(h *Health) string {
	if h == nil {
		return ""
	}
	return h.Live + " " + h.PreUpdate
}
