package mirror

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The subscription store, in a SEPARATE database file from the cache. That
// separation is why this file exists instead of two more tables in schema.sql:
// the cache is nuked whenever a resource changes, which is safe only because
// every row in it is a copy of something upstream still has. A registration is
// a copy of nothing, and nuking one stops telling somebody who asked, silently.

// subscriptionSchema has no fingerprint and nothing nukes it: a change here has
// to be one an existing file survives.
const subscriptionSchema = `
CREATE TABLE IF NOT EXISTS subscription (
	id           TEXT PRIMARY KEY,
	principal    TEXT NOT NULL,
	url          TEXT NOT NULL,
	secret       TEXT NOT NULL,
	events       TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	last_ok      INTEGER,
	failures     INTEGER NOT NULL DEFAULT 0,
	disabled     INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS subscription_principal ON subscription (principal);
`

// Subscription is one consumer asking to be told.
type Subscription struct {
	ID        string `json:"id"`
	Principal string `json:"principal"`
	URL       string `json:"url"`
	// Secret is never rendered. Create returns it once, the only chance to.
	Secret    string    `json:"-"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"created_at"`
	LastOK    time.Time `json:"last_ok,omitempty"`
	Failures  int       `json:"failures"`
	Disabled  bool      `json:"disabled"`
	LastError string    `json:"last_error,omitempty"`
}

// Subscriptions is the config database.
type Subscriptions struct {
	db   *sql.DB
	path string
}

// subscriptionsPath keeps the config database beside the cache unless a spec
// names one: a bare working-directory default splits the pair, or fails.
func subscriptionsPath(declared, cacheDB string) string {
	if strings.TrimSpace(declared) != "" {
		return declared
	}
	return strings.TrimSuffix(cacheDB, ".db") + "-subscriptions.db"
}

// OpenSubscriptions opens or creates the config database.
func OpenSubscriptions(path string) (*Subscriptions, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("<notify>: db path resolved to nothing")
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(subscriptionSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create subscription schema in %q: %w", path, err)
	}
	return &Subscriptions{db: db, path: path}, nil
}

// Path is the file this store lives in, for the dashboard to name.
func (s *Subscriptions) Path() string { return s.path }

// Close releases the database.
func (s *Subscriptions) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

// Create registers one subscription and returns it with its secret.
//
// The secret is minted here rather than accepted from the caller. A subscriber
// choosing their own would be free to choose a weak one, and the signature is
// the only thing that tells their endpoint a notification came from this mirror.
func (s *Subscriptions) Create(ctx context.Context, principal, url string, events []string) (Subscription, error) {
	sub := Subscription{
		ID:        newID(),
		Principal: principal,
		URL:       url,
		Secret:    newSecret(),
		Events:    events,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscription (id, principal, url, secret, events, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Principal, sub.URL, sub.Secret, strings.Join(events, ","), sub.CreatedAt.Unix())
	if err != nil {
		return Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	return sub, nil
}

// Delete removes one subscription belonging to a principal.
//
// The principal is part of the predicate, not checked afterwards: a caller must
// not be able to delete somebody else's registration by naming its id.
func (s *Subscriptions) Delete(ctx context.Context, principal, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM subscription WHERE id = ? AND principal = ?`, id, principal)
	if err != nil {
		return false, fmt.Errorf("delete subscription: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ByPrincipal lists one caller's own subscriptions.
func (s *Subscriptions) ByPrincipal(ctx context.Context, principal string) ([]Subscription, error) {
	return s.query(ctx, `SELECT id, principal, url, events, created_at, last_ok, failures, disabled, last_error
		FROM subscription WHERE principal = ? ORDER BY created_at`, principal)
}

// All lists every subscription, for the operator surface.
func (s *Subscriptions) All(ctx context.Context) ([]Subscription, error) {
	return s.query(ctx, `SELECT id, principal, url, events, created_at, last_ok, failures, disabled, last_error
		FROM subscription ORDER BY created_at`)
}

// Matching lists the live subscriptions that asked for this event type.
//
// An empty events list means every type: a subscriber who named nothing asked
// for everything, which is the reading that cannot silently drop a delivery
// somebody wanted.
func (s *Subscriptions) Matching(ctx context.Context, eventType string) ([]Subscription, error) {
	rows, err := s.query(ctx, `SELECT id, principal, url, events, created_at, last_ok, failures, disabled, last_error
		FROM subscription WHERE disabled = 0`)
	if err != nil {
		return nil, err
	}
	out := make([]Subscription, 0, len(rows))
	for _, sub := range rows {
		if len(sub.Events) > 0 && !containsString(sub.Events, eventType) {
			continue
		}
		secret, err := s.secretOf(ctx, sub.ID)
		if err != nil {
			return nil, err
		}
		sub.Secret = secret
		out = append(out, sub)
	}
	return out, nil
}

// Count is how many registrations exist.
func (s *Subscriptions) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscription`).Scan(&n)
	return n, err
}

// MarkDelivered records a successful notification and clears the failure run.
func (s *Subscriptions) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription SET last_ok = ?, failures = 0, last_error = '' WHERE id = ?`,
		time.Now().Unix(), id)
	return err
}

// MarkFailed counts one failure and parks the subscription at the limit.
func (s *Subscriptions) MarkFailed(ctx context.Context, id, cause string, limit int) (int, error) {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription SET failures = failures + 1, last_error = ?,
		 disabled = CASE WHEN failures + 1 >= ? THEN 1 ELSE disabled END WHERE id = ?`,
		cause, limit, id)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT failures FROM subscription WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (s *Subscriptions) secretOf(ctx context.Context, id string) (string, error) {
	var secret string
	err := s.db.QueryRowContext(ctx, `SELECT secret FROM subscription WHERE id = ?`, id).Scan(&secret)
	if err != nil {
		return "", fmt.Errorf("read subscription secret: %w", err)
	}
	return secret, nil
}

func (s *Subscriptions) query(ctx context.Context, stmt string, args ...any) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read subscriptions: %w", err)
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		var (
			sub      Subscription
			events   string
			created  int64
			lastOK   sql.NullInt64
			disabled int
		)
		if err := rows.Scan(&sub.ID, &sub.Principal, &sub.URL, &events, &created,
			&lastOK, &sub.Failures, &disabled, &sub.LastError); err != nil {
			return nil, fmt.Errorf("read subscriptions: %w", err)
		}
		if events != "" {
			sub.Events = strings.Split(events, ",")
		}
		sub.CreatedAt = time.Unix(created, 0).UTC()
		if lastOK.Valid {
			sub.LastOK = time.Unix(lastOK.Int64, 0).UTC()
		}
		sub.Disabled = disabled != 0
		out = append(out, sub)
	}
	return out, rows.Err()
}

func newID() string     { return randomHex(8) }
func newSecret() string { return randomHex(32) }

// randomHex returns n random bytes as hex. A failed read panics: a predictable
// secret is worse than no subscription, and a fallback would return one.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("api-mirror: the system random source failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
