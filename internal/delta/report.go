// report.go holds Side.

package delta

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Side is one target's part of a record.
type Side struct {
	Status      int               `json:"status,omitempty"`
	Millis      int64             `json:"ms"`
	Error       string            `json:"error,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Record is one request compared against one mirror.
type Record struct {
	Time    time.Time `json:"time"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	Query   string    `json:"query,omitempty"`
	Mirror  string    `json:"mirror"`
	Truth   Side      `json:"truth"`
	Answer  Side      `json:"answer"`
	Diffs   []Diff    `json:"diffs,omitempty"`
	Ignored []Diff    `json:"ignored,omitempty"`
}

// Differs reports whether the record holds a difference no rule ignores.
func (r *Record) Differs() bool { return len(r.Diffs) > 0 }

// maxRows bounds the summary.
const maxRows = 10000

type rowKey struct {
	method, path, mirror string
	kind                 Kind
	shape                string
}

// Row counts one class of difference on one request path.
type Row struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Mirror  string `json:"mirror"`
	Kind    Kind   `json:"kind"`
	Shape   string `json:"shape,omitempty"`
	Count   int    `json:"count"`
	Example Diff   `json:"example"`
}

// RuleCount reports how much one ignore rule hides. A rule at zero matches
// nothing in this run, so it is either stale or untested.
type RuleCount struct {
	Index   int    `json:"index"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason,omitempty"`
	Matched int    `json:"matched"`
}

// Summary is the aggregate of every record this process saw.
type Summary struct {
	Exchanges   int         `json:"exchanges"`
	Matched     int         `json:"matched"`
	Differing   int         `json:"differing"`
	Errors      int         `json:"errors"`
	Uncompared  int         `json:"uncompared"`
	Rows        []Row       `json:"rows"`
	DroppedRows int         `json:"dropped_rows"`
	Rules       []RuleCount `json:"rules"`
	Evicted     int         `json:"evicted_recent"`
}

// Reporter logs each record, appends it to the NDJSON report and keeps the
// summary and a ring of recent records in memory.
type Reporter struct {
	mu      sync.Mutex
	log     *slog.Logger
	file    *os.File
	out     *bufio.Writer
	recent  []Record
	next    int
	evicted int
	sum     Summary
	rows    map[rowKey]*Row
	rules   []*Rule
}

// NewReporter opens path for appending when it is not empty.
func NewReporter(log *slog.Logger, path string, recent int, rules []*Rule) (*Reporter, error) {
	r := &Reporter{log: log, recent: make([]Record, 0, recent), rows: map[rowKey]*Row{}, rules: rules}
	r.sum.Rules = make([]RuleCount, len(rules))
	for i, rule := range rules {
		r.sum.Rules[i] = RuleCount{Index: i, Rule: rule.Describe(), Reason: rule.Reason}
	}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("report: %w", err)
		}
		r.file = f
		r.out = bufio.NewWriter(f)
	}
	return r, nil
}

// Uncompared counts a request that went to the served target alone.
func (r *Reporter) Uncompared() {
	r.mu.Lock()
	r.sum.Uncompared++
	r.mu.Unlock()
}

// Add takes one record.
func (r *Reporter) Add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logRecord(rec)
	if r.out != nil {
		line := mustJSON(rec)
		if _, err := r.out.Write(append(line, '\n')); err != nil {
			r.log.Error("report write failed; this record is not in the report file", "err", err, "method", rec.Method, "path", rec.Path)
		} else if err := r.out.Flush(); err != nil {
			r.log.Error("report flush failed", "err", err)
		}
	}
	r.addRecent(rec)
	r.count(rec)
}

func (r *Reporter) logRecord(rec Record) {
	if !rec.Differs() {
		r.log.Debug("match", "method", rec.Method, "path", rec.Path, "mirror", rec.Mirror, "ignored", len(rec.Ignored))
		return
	}
	for _, d := range rec.Diffs {
		level := slog.LevelWarn
		if d.Kind == KindError {
			level = slog.LevelError
		}
		r.log.Log(context.Background(), level, "delta", "method", rec.Method, "path", rec.Path, "mirror", rec.Mirror,
			"kind", d.Kind, "at", d.Path, "truth", string(d.Truth), "mirror_value", string(d.Mirror))
	}
}

func (r *Reporter) addRecent(rec Record) {
	if cap(r.recent) == 0 {
		return
	}
	if len(r.recent) < cap(r.recent) {
		r.recent = append(r.recent, rec)
		return
	}
	r.recent[r.next] = rec
	r.next = (r.next + 1) % cap(r.recent)
	r.evicted++
}

func (r *Reporter) count(rec Record) {
	r.sum.Exchanges++
	switch {
	case hasKind(rec.Diffs, KindError):
		r.sum.Errors++
	case rec.Differs():
		r.sum.Differing++
	default:
		r.sum.Matched++
	}
	for _, d := range rec.Ignored {
		r.sum.Rules[*d.Rule].Matched++
	}
	for _, d := range rec.Diffs {
		k := rowKey{rec.Method, rec.Path, rec.Mirror, d.Kind, d.Shape()}
		if row, ok := r.rows[k]; ok {
			row.Count++
			continue
		}
		if len(r.rows) >= maxRows {
			r.sum.DroppedRows++
			if r.sum.DroppedRows == 1 || r.sum.DroppedRows%1000 == 0 {
				r.log.Error("summary is full; new difference classes are counted but not kept",
					"max_rows", maxRows, "dropped", r.sum.DroppedRows)
			}
			continue
		}
		r.rows[k] = &Row{Method: rec.Method, Path: rec.Path, Mirror: rec.Mirror, Kind: d.Kind, Shape: k.shape, Count: 1, Example: d}
	}
}

func hasKind(ds []Diff, k Kind) bool {
	for _, d := range ds {
		if d.Kind == k {
			return true
		}
	}
	return false
}

// Summary returns a copy of the aggregate, rows sorted by count.
func (r *Reporter) Summary() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sum
	s.Evicted = r.evicted
	s.Rules = append([]RuleCount(nil), r.sum.Rules...)
	s.Rows = make([]Row, 0, len(r.rows))
	for _, row := range r.rows {
		s.Rows = append(s.Rows, *row)
	}
	sort.Slice(s.Rows, func(i, j int) bool {
		a, b := s.Rows[i], s.Rows[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return fmt.Sprint(a.Method, a.Path, a.Mirror, a.Kind, a.Shape) < fmt.Sprint(b.Method, b.Path, b.Mirror, b.Kind, b.Shape)
	})
	return s
}

// Recent returns up to limit records, newest first.
func (r *Reporter) Recent(limit int) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.recent)
	out := make([]Record, 0, min(n, limit))
	for i := 0; i < n && len(out) < limit; i++ {
		out = append(out, r.recent[(r.next-1-i+2*n)%n])
	}
	return out
}

// Close flushes and closes the report file.
func (r *Reporter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	ferr := r.out.Flush()
	cerr := r.file.Close()
	r.file = nil
	if ferr != nil {
		return ferr
	}
	return cerr
}

// WriteText renders the summary for a terminal.
func (s Summary) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "exchanges %d  matched %d  differing %d  errors %d  uncompared %d\n",
		s.Exchanges, s.Matched, s.Differing, s.Errors, s.Uncompared)
	if s.DroppedRows > 0 {
		fmt.Fprintf(&b, "WARNING: %d difference classes were counted but not kept; the summary was full\n", s.DroppedRows)
	}
	if len(s.Rules) > 0 {
		b.WriteString("\nignore rules:\n")
		for _, rc := range s.Rules {
			fmt.Fprintf(&b, "  [%d] %-50s %6d ignored", rc.Index, rc.Rule, rc.Matched)
			if rc.Matched == 0 {
				b.WriteString("  (matched nothing)")
			}
			if rc.Reason != "" {
				b.WriteString("  # " + rc.Reason)
			}
			b.WriteByte('\n')
		}
	}
	if len(s.Rows) > 0 {
		b.WriteString("\ndifferences:\n")
		for _, row := range s.Rows {
			fmt.Fprintf(&b, "  %5dx %s %s [%s] %s %s\n", row.Count, row.Method, row.Path, row.Mirror, row.Kind, row.Shape)
			fmt.Fprintf(&b, "         truth=%s mirror=%s\n", orDash(row.Example.Truth), orDash(row.Example.Mirror))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func orDash(v json.RawMessage) string {
	if len(v) == 0 {
		return "-"
	}
	return string(v)
}
