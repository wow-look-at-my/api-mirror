package mirror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// refusalDDL holds the document each stored refusal came with.
const refusalDDL = `CREATE TABLE mirror_refusal (
	kind TEXT NOT NULL,
	key  TEXT NOT NULL,
	body TEXT NOT NULL,
	PRIMARY KEY (kind, key)
);
`

// SetRefusalBody records the document a refusal came with. An empty body
// removes the record, so an older document never answers for a newer refusal.
func (s *Store) SetRefusalBody(ctx context.Context, kind, key, body string) error {
	var err error
	if body == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM mirror_refusal WHERE kind = ? AND key = ?`, kind, key)
	} else {
		_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mirror_refusal (kind, key, body) VALUES (?, ?, ?)`, kind, key, body)
	}
	if err != nil {
		return fmt.Errorf("record refusal %s/%s: %w", kind, key, err)
	}
	return nil
}

// RefusalBody reads the document a stored refusal came with. None is "".
func (s *Store) RefusalBody(ctx context.Context, kind, key string) (string, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM mirror_refusal WHERE kind = ? AND key = ?`, kind, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read refusal %s/%s: %w", kind, key, err)
	}
	return body, nil
}

// DeleteFreshnessUnder forgets the bookkeeping of every key of a kind whose
// text starts with prefix. substr compares bytes, so no character in a key
// acts as a wildcard.
func (s *Store) DeleteFreshnessUnder(ctx context.Context, kind, prefix string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM mirror_freshness WHERE kind = ? AND substr(key, 1, length(?)) = ?`, kind, prefix, prefix)
	if err != nil {
		return 0, fmt.Errorf("delete freshness %s/%s*: %w", kind, prefix, err)
	}
	return res.RowsAffected()
}
