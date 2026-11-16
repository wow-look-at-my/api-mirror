// compare.go holds errDiffers.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/api-mirror/internal/delta"
)

// errDiffers makes the process exit 1 without a message: the report already says what differs.
var errDiffers = errors.New("differences found")

var compareFlags struct {
	method  string
	headers []string
	format  string
}

var compareCmd = &cobra.Command{
	Use:   "compare PATH...",
	Short: "Send each path to every target once and report the differences",
	Long: `compare sends each request path, with its query, to the truth and to every
mirror, and prints what differs. It exits 1 when any difference survives the
ignore rules, so a script or a test can gate on it.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		header := http.Header{}
		for _, h := range compareFlags.headers {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				return fmt.Errorf("--header %q: want Name: value", h)
			}
			header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		if compareFlags.format != "text" && compareFlags.format != "json" {
			return fmt.Errorf("--format %q: want text or json", compareFlags.format)
		}
		rep, err := delta.NewReporter(logger(), cfg.Report, cfg.Recent, cfg.Ignore)
		if err != nil {
			return err
		}
		defer rep.Close()
		d := delta.New(cfg, rep, logger())

		var recs []delta.Record
		for _, a := range args {
			u, err := url.Parse(a)
			if err != nil || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
				return fmt.Errorf("%q: want a path such as /repos/o/r?per_page=5", a)
			}
			recs = append(recs, d.Compare(cmd.Context(), &delta.Inbound{
				Method: strings.ToUpper(compareFlags.method),
				Path:   u.EscapedPath(),
				Query:  u.RawQuery,
				Header: header,
			})...)
		}
		out := cmd.OutOrStdout()
		if compareFlags.format == "json" {
			if err := writeRecordsJSON(out, recs, rep.Summary()); err != nil {
				return err
			}
		} else if err := writeRecordsText(out, recs, rep.Summary()); err != nil {
			return err
		}
		for _, r := range recs {
			if r.Differs() {
				return errDiffers
			}
		}
		return nil
	},
}

func init() {
	f := compareCmd.Flags()
	f.StringVarP(&compareFlags.method, "method", "X", http.MethodGet, "request method")
	f.StringArrayVarP(&compareFlags.headers, "header", "H", nil, "a request header as 'Name: value'; repeatable")
	f.StringVar(&compareFlags.format, "format", "text", "output format: text or json")
	rootCmd.AddCommand(compareCmd)
}

func writeRecordsText(w io.Writer, recs []delta.Record, s delta.Summary) error {
	var b strings.Builder
	for _, r := range recs {
		state := "match"
		if r.Differs() {
			state = fmt.Sprintf("%d differences", len(r.Diffs))
		}
		fmt.Fprintf(&b, "%s %s [%s] truth=%d mirror=%d: %s", r.Method, pathQuery(r), r.Mirror, r.Truth.Status, r.Answer.Status, state)
		if len(r.Ignored) > 0 {
			fmt.Fprintf(&b, " (%d ignored)", len(r.Ignored))
		}
		b.WriteByte('\n')
		for _, d := range r.Diffs {
			fmt.Fprintf(&b, "  %-8s %s\n    truth:  %s\n    mirror: %s\n", d.Kind, d.Path, dash(d.Truth), dash(d.Mirror))
		}
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return s.WriteText(w)
}

func pathQuery(r delta.Record) string {
	if r.Query == "" {
		return r.Path
	}
	return r.Path + "?" + r.Query
}

func dash(v json.RawMessage) string {
	if len(v) == 0 {
		return "(absent)"
	}
	return string(v)
}

func writeRecordsJSON(w io.Writer, recs []delta.Record, s delta.Summary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	return enc.Encode(struct {
		Records []delta.Record `json:"records"`
		Summary delta.Summary  `json:"summary"`
	}{recs, s})
}

func exitCode(err error) int {
	if errors.Is(err, errDiffers) {
		return 1
	}
	fmt.Fprintln(os.Stderr, "api-mirror-delta:", err)
	return 2
}
