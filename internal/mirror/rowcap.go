package mirror

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// defaultMaxRows is the per-table ceiling when the command line names none.
const defaultMaxRows int64 = 1_000_000

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
