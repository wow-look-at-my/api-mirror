// rowcap.go holds defaultMaxRows.

package mirror

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// defaultMaxRows bounds every resource table when a spec names no cap.
const defaultMaxRows = 1_000_000

// rowCapInterval throttles the cap check per table.
const rowCapInterval = 5 * time.Minute

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
// per interval. What it drops is logged at error level with the count. A
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
