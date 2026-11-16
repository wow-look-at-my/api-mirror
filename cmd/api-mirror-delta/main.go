// Command api-mirror-delta holds one or more API mirrors against the API
// they mirror and reports every difference between their answers.
package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/api-mirror/internal/delta"
)

var flags struct {
	config  string
	truth   string
	mirrors []string
	ignore  []string
	serve   string
	report  string
	verbose bool
}

var rootCmd = &cobra.Command{
	Use:           "api-mirror-delta",
	Short:         "Compare API mirrors against the API they mirror",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&flags.config, "config", "c", "", "YAML config file")
	pf.StringVar(&flags.truth, "truth", "", "base URL of the ground-truth API; replaces the config's")
	pf.StringArrayVar(&flags.mirrors, "mirror", nil, "a mirror as name=url; repeatable; replaces the config's mirrors")
	pf.StringArrayVar(&flags.ignore, "ignore", nil, "a JSONPath whose differences are ignored; repeatable; adds to the config's rules")
	pf.StringVar(&flags.serve, "serve", "", "the target whose answer the client gets: truth or a mirror name")
	pf.StringVar(&flags.report, "report", "", "append every record to this NDJSON file")
	pf.BoolVarP(&flags.verbose, "verbose", "v", false, "also log every request that matched")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	if flags.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// loadConfig reads the config file when one is named, then applies the
// command-line flags over it.
func loadConfig() (*delta.Config, error) {
	cfg := &delta.Config{}
	if flags.config != "" {
		c, err := delta.LoadConfig(flags.config)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	if flags.truth != "" {
		cfg.Truth.URL = flags.truth
	}
	if len(flags.mirrors) > 0 {
		cfg.Mirrors = nil
		for _, m := range flags.mirrors {
			t, err := parseMirror(m)
			if err != nil {
				return nil, err
			}
			cfg.Mirrors = append(cfg.Mirrors, t)
		}
	}
	for _, p := range flags.ignore {
		cfg.Ignore = append(cfg.Ignore, &delta.Rule{Path: p, Reason: "--ignore"})
	}
	if flags.serve != "" {
		cfg.Serve = flags.serve
	}
	if flags.report != "" {
		cfg.Report = flags.report
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config:\n%w", err)
	}
	return cfg, nil
}

// parseMirror reads name=url. A bare URL takes its host as the name.
func parseMirror(s string) (delta.Target, error) {
	if name, u, ok := strings.Cut(s, "="); ok && !strings.Contains(name, "/") {
		return delta.Target{Name: name, URL: u}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return delta.Target{}, fmt.Errorf("--mirror %q: want name=url or an absolute URL", s)
	}
	return delta.Target{Name: u.Host, URL: s}, nil
}
