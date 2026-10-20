package mirror

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
)

// The subscription store, in a SEPARATE database file from the cache.

//go:embed subscription_schema.sql
var subscriptionSchema string

// subscriptionColumns are added to a file an older build created. Each
// is additive with a default, so every existing registration survives.
var subscriptionColumns = []struct{ name, ddl string }{
	{"scopes", `ALTER TABLE subscription ADD COLUMN scopes TEXT NOT NULL DEFAULT ''`},
	{"updated_at", `ALTER TABLE subscription ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0`},
	{"last_failure", `ALTER TABLE subscription ADD COLUMN last_failure INTEGER`},
	{"disabled_reason", `ALTER TABLE subscription ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''`},
}

// subscriptionSelect is the column list every read uses.
const subscriptionSelect = `SELECT id, principal, url, events, scopes, created_at, updated_at,
	last_ok, last_failure, failures, disabled, last_error, disabled_reason FROM subscription`

// minCallerSecret is the shortest secret a subscriber may choose.
const minCallerSecret = 16

// Subscription is consumer asking to be told.
type Subscription struct {
	ID        string `json:"id"`
	Principal string `json:"principal"`
	URL       string `json:"url"`
	// Secret is never rendered. Create returns it, the only chance to.
	Secret string   `json:"-"`
	Events []string `json:"events"`
	// Scopes limits the subscription to deliveries whose scope is listed. Empty means every scope the subscriber can see.
	Scopes      []string  `json:"scopes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	LastOK      time.Time `json:"last_ok,omitempty"`
	LastFailure time.Time `json:"last_failure,omitempty"`
	Failures    int       `json:"failures"`
	Disabled    bool      `json:"disabled"`
	LastError   string    `json:"last_error,omitempty"`
	// DisabledReason says why a parked subscription stopped. A subscriber that turned itself off has none.
	DisabledReason string `json:"disabled_reason,omitempty"`
}

// SubscriptionPatch changes the fields it sets and leaves the rest alone.
type SubscriptionPatch struct {
	URL    *string
	Secret *string
	Events *[]string
	Scopes *[]string
	// Active true re-enables a parked subscription and clears its failure run.
	Active *bool
}

// Subscriptions is the config database.
type Subscriptions struct {
	db   *sql.DB
	path string
}

// subscriptionsPath keeps the config database beside the cache unless a spec
// names: a bare working-directory default splits the pair, or fails.
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
	if err := addSubscriptionColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("upgrade subscription schema in %q: %w", path, err)
	}
	return &Subscriptions{db: db, path: path}, nil
}

func addSubscriptionColumns(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('subscription')`)
	if err != nil {
		return err
	}
	have := set.New[string]()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have.Add(name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range subscriptionColumns {
		if have.Contains(col.name) {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", col.name, err)
		}
	}
	return nil
}

// CheckSecret refuses a subscriber's own secret that is too short to trust.
// An empty secret is fine: Create mints one.
func CheckSecret(secret string) error {
	if secret != "" && len(secret) < minCallerSecret {
		return errBadCallback(fmt.Sprintf("a secret must be at least %d characters, or omitted so the mirror mints one", minCallerSecret))
	}
	return nil
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

// Create registers subscription and returns it with its secret.
//
// An empty secret is minted here. A subscriber's own secret is accepted
// when CheckSecret passes it, since a subscriber that moves from another
// mirror keeps the secret its endpoint already verifies.
func (s *Subscriptions) Create(ctx context.Context, principal, url, secret string, events, scopes []string) (Subscription, error) {
	if secret == "" {
		secret = newSecret()
	}
	now := time.Now().UTC()
	sub := Subscription{
		ID:        newID(),
		Principal: principal,
		URL:       url,
		Secret:    secret,
		Events:    events,
		Scopes:    scopes,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscription (id, principal, url, secret, events, scopes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Principal, sub.URL, sub.Secret, strings.Join(events, ","), strings.Join(scopes, ","), now.Unix(), now.Unix())
	if err != nil {
		return Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	return sub, nil
}

// adoptOutcome is what adopt did with one registration of another store.
type adoptOutcome int

const (
	adoptImported adoptOutcome = iota
	adoptPresent
	adoptIDTaken
)

// adopt stores a registration that another store issued, under its own id,
// with every field as given.
//
// The import ledger records the source id. A row the ledger names is never
// written again, so a subscriber deleted here stays deleted.
func (s *Subscriptions) adopt(ctx context.Context, source, sourceID string, sub Subscription) (adoptOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscription_import WHERE source = ? AND source_id = ?`,
		source, sourceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("read import ledger: %w", err)
	}
	if n > 0 {
		return adoptPresent, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscription WHERE id = ?`, sub.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("read subscription: %w", err)
	}
	if n > 0 {
		return adoptIDTaken, nil
	}
	if _, err := tx.ExecContext(ctx,
		adoptSubscriptionSQL,
		sub.ID, sub.Principal, sub.URL, sub.Secret, strings.Join(sub.Events, ","), strings.Join(sub.Scopes, ","),
		sub.CreatedAt.Unix(), sub.UpdatedAt.Unix(), unixOrNull(sub.LastOK), unixOrNull(sub.LastFailure),
		sub.Failures, boolInt(sub.Disabled), sub.LastError, sub.DisabledReason); err != nil {
		return 0, fmt.Errorf("adopt subscription %s: %w", sub.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO subscription_import (source, source_id, subscription_id, imported_at) VALUES (?, ?, ?, ?)`,
		source, sourceID, sub.ID, time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("write import ledger: %w", err)
	}
	return adoptImported, tx.Commit()
}

func unixOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get reads one subscription that belongs to a principal. A subscription
// of another principal reads as absent.
func (s *Subscriptions) Get(ctx context.Context, principal, id string) (*Subscription, error) {
	subs, err := s.query(ctx, subscriptionSelect+` WHERE id = ? AND principal = ?`, id, principal)
	if err != nil || len(subs) == 0 {
		return nil, err
	}
	return &subs[0], nil
}

// Update applies a patch to one subscription of a principal, and reports
// whether the subscription exists.
func (s *Subscriptions) Update(ctx context.Context, principal, id string, p SubscriptionPatch) (bool, error) {
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Unix()}
	if p.URL != nil {
		sets, args = append(sets, "url = ?"), append(args, *p.URL)
	}
	if p.Secret != nil {
		sets, args = append(sets, "secret = ?"), append(args, *p.Secret)
	}
	if p.Events != nil {
		sets, args = append(sets, "events = ?"), append(args, strings.Join(*p.Events, ","))
	}
	if p.Scopes != nil {
		sets, args = append(sets, "scopes = ?"), append(args, strings.Join(*p.Scopes, ","))
	}
	if p.Active != nil {
		if *p.Active {
			sets = append(sets, "disabled = 0", "failures = 0", "last_error = ''", "disabled_reason = ''")
		} else {
			sets = append(sets, "disabled = 1")
		}
	}
	args = append(args, id, principal)
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscription SET `+strings.Join(sets, ", ")+` WHERE id = ? AND principal = ?`, args...)
	if err != nil {
		return false, fmt.Errorf("update subscription: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Delete removes subscription belonging to a principal.
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

// Reactivate re-enables a parked subscription of a principal's and clears its
// failure run.
func (s *Subscriptions) Reactivate(ctx context.Context, principal, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE subscription SET disabled = 0, failures = 0, last_error = '' WHERE id = ? AND principal = ?`, id, principal)
	if err != nil {
		return false, fmt.Errorf("reactivate subscription: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ByPrincipal lists caller's own subscriptions.
func (s *Subscriptions) ByPrincipal(ctx context.Context, principal string) ([]Subscription, error) {
	return s.query(ctx, subscriptionSelect+` WHERE principal = ? ORDER BY created_at`, principal)
}

// All lists every subscription, for the operator surface.
func (s *Subscriptions) All(ctx context.Context) ([]Subscription, error) {
	return s.query(ctx, subscriptionSelect+` ORDER BY created_at`)
}

// Matching lists the live subscriptions that asked for this event type.
//
// An empty events list means every type. A subscriber who named nothing asked
// for everything, which is the reading that cannot silently drop a delivery
// somebody wanted.
func (s *Subscriptions) Matching(ctx context.Context, eventType string) ([]Subscription, error) {
	rows, err := s.query(ctx, subscriptionSelect+` WHERE disabled = 0`)
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

// MarkFailed counts failure and parks the subscription at the limit.
func (s *Subscriptions) MarkFailed(ctx context.Context, id, cause string, limit int) (int, error) {
	_, err := s.db.ExecContext(ctx,
		markFailedSQL,
		cause, time.Now().Unix(), limit, limit, id)
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
			sub              Subscription
			events, scopes   string
			created, updated int64
			lastOK, lastFail sql.NullInt64
			disabled         int
		)
		if err := rows.Scan(&sub.ID, &sub.Principal, &sub.URL, &events, &scopes, &created, &updated,
			&lastOK, &lastFail, &sub.Failures, &disabled, &sub.LastError, &sub.DisabledReason); err != nil {
			return nil, fmt.Errorf("read subscriptions: %w", err)
		}
		if events != "" {
			sub.Events = strings.Split(events, ",")
		}
		if scopes != "" {
			sub.Scopes = strings.Split(scopes, ",")
		}
		sub.CreatedAt = time.Unix(created, 0).UTC()
		if updated > 0 {
			sub.UpdatedAt = time.Unix(updated, 0).UTC()
		}
		if lastOK.Valid {
			sub.LastOK = time.Unix(lastOK.Int64, 0).UTC()
		}
		if lastFail.Valid {
			sub.LastFailure = time.Unix(lastFail.Int64, 0).UTC()
		}
		sub.Disabled = disabled != 0
		out = append(out, sub)
	}
	return out, rows.Err()
}

func newID() string     { return randomHex(8) }
func newSecret() string { return randomHex(32) }

// randomHex returns n random bytes as hex. A failed read panics: a predictable
// secret is worse than no subscription, and a fallback would return.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("api-mirror: the system random source failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
