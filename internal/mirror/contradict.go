package mirror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Contradiction names a column of ANOTHER resource that states this
// resource's value too, arriving on a different delivery. A stored row that a
// later row of that resource contradicts is refetched instead of served.
//
// It exists for the answer a lost delivery ruins worst: a branch tip. An open
// pull request names its base sha, and a pull arrives on its own deliveries, so
// a lost push leaves both rows disagreeing and the mirror able to see it.
type Contradiction struct {
	// Resource is the other resource, Field its column holding the value.
	Resource string
	Field    string
	// Against is this resource's column the value is compared with.
	Against string
	// On joins both: each of this resource's keys to the other's column.
	On []ContradictionJoin
	// Only narrows the other resource's rows to those whose column equals a
	// constant, such as an open state.
	Only []ContradictionOnly
}

// ContradictionJoin maps a key of this resource to a column of the other.
type ContradictionJoin struct {
	Key    string
	Column string
}

// ContradictionOnly holds the other resource's rows to a single column value.
type ContradictionOnly struct {
	Column string
	Value  string
}

func buildContradiction(n *node) (*Contradiction, error) {
	if err := checkAttrs(n, "resource", "field", "against"); err != nil {
		return nil, err
	}
	c := &Contradiction{Resource: n.Attr("resource"), Field: n.Attr("field"), Against: n.Attr("against")}
	for _, child := range n.Children() {
		switch child.Name() {
		case "on":
			if err := checkAttrs(child, "key", "column"); err != nil {
				return nil, err
			}
			c.On = append(c.On, ContradictionJoin{Key: child.Attr("key"), Column: child.Attr("column")})
		case "only":
			if err := checkAttrs(child, "column", "value"); err != nil {
				return nil, err
			}
			c.Only = append(c.Only, ContradictionOnly{Column: child.Attr("column"), Value: child.Attr("value")})
		default:
			return nil, fmt.Errorf("<contradicted-by>: unexpected child element <%s>", child.Name())
		}
	}
	return c, nil
}

// validate checks every name the rule uses is a real column, and that the join
// covers every key: a partial join compares against rows about something else.
func (c *Contradiction) validate(r *Resource, byName map[string]*Resource) error {
	other, ok := byName[c.Resource]
	if !ok {
		return fmt.Errorf("resource %q: <contradicted-by> names resource %q, which is not declared", r.Name, c.Resource)
	}
	if r.whole() || other.whole() {
		return fmt.Errorf("resource %q: <contradicted-by> compares columns, and a document resource has none", r.Name)
	}
	if !slices.Contains(columnsOf(other), c.Field) {
		return fmt.Errorf("resource %q: <contradicted-by> field %q is not a column of %q", r.Name, c.Field, c.Resource)
	}
	if !slices.Contains(columnsOf(r), c.Against) {
		return fmt.Errorf("resource %q: <contradicted-by> against %q is not a column of it", r.Name, c.Against)
	}
	for _, k := range r.Keys {
		i := slices.IndexFunc(c.On, func(j ContradictionJoin) bool { return j.Key == k.Name })
		if i < 0 {
			return fmt.Errorf("resource %q: <contradicted-by> joins no column to key %q", r.Name, k.Name)
		}
		if !slices.Contains(columnsOf(other), c.On[i].Column) {
			return fmt.Errorf("resource %q: <contradicted-by> joins key %q to %q, which is not a column of %q", r.Name, k.Name, c.On[i].Column, c.Resource)
		}
	}
	if len(c.On) != len(r.Keys) {
		return fmt.Errorf("resource %q: <contradicted-by> joins a key twice or a name that is not a key", r.Name)
	}
	for _, o := range c.Only {
		if !slices.Contains(columnsOf(other), o.Column) {
			return fmt.Errorf("resource %q: <contradicted-by> only %q is not a column of %q", r.Name, o.Column, c.Resource)
		}
	}
	return nil
}

// settledValues remembers, per stored key, the contradicting value a refetch
// already answered. A pull's base sha legitimately lags its branch, so the
// same lagging value absorbed again is not new evidence.
type settledValues struct {
	mu   sync.Mutex
	seen map[string]string
}

func (s *settledValues) settled(key, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[key] == value
}

func (s *settledValues) settle(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]string)
	}
	s.seen[key] = value
}

// contradicted reports a value another resource states for this key that
// disagrees with the stored row, came from a write after it, and has not
// already been settled by a refetch. Empty means serve the row as stored.
func (s *Store) contradicted(ctx context.Context, res *Resource, key map[string]string, settled *settledValues) (string, error) {
	c := res.Contradiction
	where, args := keyPredicate(res, key)
	if where == "" || len(key) != len(res.Keys) {
		return "", nil
	}
	var stored sql.NullString
	var writtenAt int64
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s, mirror_written_at FROM %s WHERE %s`,
		c.Against, resourceTable(res.Name), where), args...).Scan(&stored, &writtenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("contradiction %s: %w", res.Name, err)
	}
	terms := []string{"mirror_written_at > ?", c.Field + " IS NOT NULL", c.Field + " != ''", c.Field + " != ?"}
	params := []any{writtenAt, stored.String}
	for _, j := range c.On {
		terms = append(terms, j.Column+" = ?")
		params = append(params, key[j.Key])
	}
	for _, o := range c.Only {
		terms = append(terms, o.Column+" = ?")
		params = append(params, o.Value)
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT %s FROM %s WHERE %s`,
		c.Field, resourceTable(c.Resource), strings.Join(terms, " AND ")), params...)
	if err != nil {
		return "", fmt.Errorf("contradiction %s by %s: %w", res.Name, c.Resource, err)
	}
	defer rows.Close()
	id := res.Name + "\x00" + keyString(res, key)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return "", fmt.Errorf("contradiction %s by %s: %w", res.Name, c.Resource, err)
		}
		if !settled.settled(id, v) {
			return v, rows.Err()
		}
	}
	return "", rows.Err()
}

// refetchIfContradicted drops the freshness of a stored row another resource
// contradicts, so the read that follows asks the upstream. A failure to check
// serves the row as stored: the check is a repair, and the row is what the
// mirror would have served without it.
func (e *Engine) refetchIfContradicted(ctx context.Context, res *Resource, key map[string]string) {
	if res.Contradiction == nil {
		return
	}
	v, err := e.store.contradicted(ctx, res, key, &e.settled)
	if err != nil {
		logf("%v", err)
		return
	}
	if v == "" {
		return
	}
	logf("%s %s: %s states %q, refetching", res.Name, keyString(res, key), res.Contradiction.Resource, v)
	e.settled.settle(res.Name+"\x00"+keyString(res, key), v)
	if err := e.store.Forget(ctx, res, key); err != nil {
		logf("forget contradicted %s: %v", res.Name, err)
	}
}
