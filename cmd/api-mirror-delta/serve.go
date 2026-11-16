// serve.go holds drainTimeout.

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/api-mirror/internal/delta"
)

// drainTimeout bounds how long a shutdown waits for comparisons in flight.
const drainTimeout = 30 * time.Second

var listen string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the comparing proxy until a signal",
	Long: `serve listens for API requests. Each request goes to the served target, and
the client gets that answer. A request the config selects to compare also goes
to the truth and to every mirror, and each mirror answer is diffed against the
truth answer. Differences are logged, appended to the report file, and
summarised at /_delta/summary.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if listen != "" {
			cfg.Listen = listen
		}
		log := logger()
		rep, err := delta.NewReporter(log, cfg.Report, cfg.Recent, cfg.Ignore)
		if err != nil {
			return err
		}
		defer rep.Close()
		d := delta.New(cfg, rep, log)

		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: d, ReadHeaderTimeout: 10 * time.Second}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		// Serve returns as soon as Shutdown starts. The drain must wait until every handler has registered its comparison.
		shutDown := make(chan struct{})
		go func() {
			defer close(shutDown)
			<-ctx.Done()
			shut, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			_ = srv.Shutdown(shut)
		}()
		log.Info("serving", "listen", ln.Addr().String(), "truth", cfg.Truth.URL, "mirrors", len(cfg.Mirrors), "serve", cfg.Serve,
			"summary", "http://"+ln.Addr().String()+delta.AdminPrefix+"summary?format=text")
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		<-shutDown
		return d.Drain(drainTimeout)
	},
}

func init() {
	serveCmd.Flags().StringVarP(&listen, "listen", "l", "", "listen address; replaces the config's (default 127.0.0.1:8090)")
	rootCmd.AddCommand(serveCmd)
}
