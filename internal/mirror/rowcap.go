package mirror

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultMaxRows is the per-table ceiling when neither the command line nor
// the spec names one.
const defaultMaxRows = 1_000_000

// rowCapInterval throttles the cap check per table.
const rowCapInterval = 5 * time.Minute

// rowCounts estimates each resource table's size, so a write pays for an exact
// count a single time the estimate crosses the ceiling. Every written row
// counts as new, so a replaced row makes the estimate high, never low.
type rowCounts struct {
	mu sync.Mutex
	n  map[string]int64
}

// over adds written rows to the estimate and reports whether an exact count is
// due. A table never counted is always due.
func (c *rowCounts) over(name string, written int, ceiling int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.n[name]
	if !ok {
		return true
	}
	n += int64(written)
	c.n[name] = n
	return n > ceiling
}

func (c *rowCounts) set(name string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[name] = n
}

// capRows evicts the oldest rows of a resource beyond the ceiling. Each evicted
// row loses its freshness too: a key that is still marked fresh after its row
// is gone reads as fresh and empty.
func (s *Store) capRows(ctx context.Context, r *Resource, written int) error {
	if !s.counts.over(r.Name, written, s.maxRows) {
		return nil
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+resourceTable(r.Name)).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", r.Name, err)
	}
	if n <= s.maxRows {
		s.counts.set(r.Name, n)
		return nil
	}
	keys, err := s.oldestKeys(ctx, r, n-s.maxRows)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := s.Delete(ctx, r, k); err != nil {
			return err
		}
		if err := s.Forget(ctx, r, k); err != nil {
			return err
		}
	}
	s.counts.set(r.Name, n-int64(len(keys)))
	return nil
}

// oldestKeys reads the full keys of the limit least recently written rows.
func (s *Store) oldestKeys(ctx context.Context, r *Resource, limit int64) ([]map[string]string, error) {
	names := keyNames(r)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM %s ORDER BY mirror_written_at LIMIT ?`,
		strings.Join(names, ", "), resourceTable(r.Name)), limit)
	if err != nil {
		return nil, fmt.Errorf("oldest %s: %w", r.Name, err)
	}
	defer rows.Close()

	var out []map[string]string
	vals := make([]string, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("oldest %s: %w", r.Name, err)
		}
		key := make(map[string]string, len(names))
		for i, name := range names {
			key[name] = vals[i]
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// rowCap keeps each resource table under a row ceiling. A TTL bounds most
// tables on its own.
type rowCap struct {
	max int
	mu  sync.Mutex
	// checked stamps each table's last check.
	checked map[string]time.Time
}

// parseMaxRows reads <mirror max-rows=>. Empty is the default; a value
// that is not a positive number fails the load.
func parseMaxRows(v string) (int, error) {
	v, err := loadTimeValue("<mirror> max-rows", v)
	if err != nil {
		return 0, err
	}
	if v == "" {
		return defaultMaxRows, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("<mirror> max-rows=%q must be a number of at least 1", v)
	}
	return n, nil
}

// enforce trims a table back to the cap, oldest writes first, at most once
// per interval. What it drops is logged at error level with the count: a
// cap that bites is a table outgrowing its design, and nobody learns that
// from a quiet trim.
func (s *Store) enforceCap(ctx context.Context, r *Resource) {
	c := s.cap
	if c == nil || c.max <= 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if last, ok := c.checked[r.Name]; ok && now.Sub(last) < rowCapInterval {
		c.mu.Unlock()
		return
	}
	c.checked[r.Name] = now
	c.mu.Unlock()

	table := resourceTable(r.Name)
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s ORDER BY mirror_written_at ASC
		 LIMIT max(0, (SELECT COUNT(*) FROM %s) - ?))`, table, table, table), c.max)
	if err != nil {
		logf("row cap %s: %v", r.Name, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logf("row cap %s: dropped the %d oldest rows to stay at %d", r.Name, n, c.max)
	}
}
