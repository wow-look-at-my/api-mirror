package mirror

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wow-look-at-my/api-mirror/internal/database/dbgen"
	_ "modernc.org/sqlite"
)

// Store is the derived SQLite database: the engine's own tables plus one table
// per declared resource.
type Store struct {
	db   *sql.DB
	path string
	q    *dbgen.Queries
	spec *Spec
	// byName resolves a resource once, so no hot path does a linear scan.
	byName map[string]*Resource
}

// Open opens (or creates) the database at path and brings it to the schema this
// spec derives.
//
// A database recording a different fingerprint is DELETED and recreated. That
// is safe because this is a cache: every row in it is a copy of something the
// upstream still has, and the cost of being wrong about that is a refetch. The
// alternative -- migrations for a cache -- is a maintenance burden paid forever
// to preserve data that is disposable by definition.
func Open(ctx context.Context, path string, spec *Spec) (*Store, error) {
	if err := spec.validateNames(); err != nil {
		return nil, err
	}
	want := spec.Fingerprint()

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	got, err := readFingerprint(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if got != "" && got != want {
		db.Close()
		if err := removeDB(path); err != nil {
			return nil, fmt.Errorf("nuke stale cache %q: %w", path, err)
		}
		if db, err = openDB(path); err != nil {
			return nil, err
		}
		got = ""
	}
	if got == "" {
		if err := applySchema(ctx, db, spec, want); err != nil {
			db.Close()
			return nil, err
		}
	}

	s := &Store{db: db, path: path, q: dbgen.New(db), spec: spec, byName: make(map[string]*Resource, len(spec.Resources))}
	for _, r := range spec.Resources {
		s.byName[r.Name] = r
	}
	return s, nil
}

func openDB(path string) (*sql.DB, error) {
	// WAL keeps a reader from blocking the writer, which matters because a
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	return db, nil
}

// removeDB deletes the database and the sidecars WAL mode leaves beside it.
// Leaving a sidecar behind resurrects part of the old database on the next open.
func removeDB(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// readFingerprint reports what schema the file on disk was built from, or ""
// when it was never built at all.
//
// The sqlite_master probe stays hand-written because a fresh file has no
// mirror_meta yet, and a generated query against a table that does not exist
// fails rather than answering "no".
func readFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='mirror_meta'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read schema state: %w", err)
	}
	fp, err := dbgen.New(db).GetMeta(ctx, metaFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read fingerprint: %w", err)
	}
	return fp, nil
}

// metaFingerprint is the mirror_meta key recording the schema this file holds.
const metaFingerprint = "fingerprint"

func applySchema(ctx context.Context, db *sql.DB, spec *Spec, fingerprint string) error {
	if _, err := db.ExecContext(ctx, spec.DDL()); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	err := dbgen.New(db).SetMeta(ctx, dbgen.SetMetaParams{Key: metaFingerprint, Value: fingerprint})
	if err != nil {
		return fmt.Errorf("record fingerprint: %w", err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the database still answers. A liveness check that only
// proves the process is scheduled reports a mirror with no store as healthy.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Resource resolves a declared resource by name.
func (s *Store) Resource(name string) (*Resource, bool) {
	r, ok := s.byName[name]
	return r, ok
}

// Meta reads one engine setting. A missing key reads as "".
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	v, err := s.q.GetMeta(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, nil
}

// SetMeta writes one engine setting.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if err := s.q.SetMeta(ctx, dbgen.SetMetaParams{Key: key, Value: value}); err != nil {
		return fmt.Errorf("write meta %q: %w", key, err)
	}
	return nil
}

// tx runs fn inside one transaction, so a multi-statement change either lands
// whole or not at all.
func (s *Store) tx(ctx context.Context, fn func(*dbgen.Queries) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err := fn(s.q.WithTx(t)); err != nil {
		return err
	}
	return t.Commit()
}

// Freshness is the fetch bookkeeping for one resource key. A zero time means
// that moment never happened.
type Freshness struct {
	Kind       string
	Key        string
	FetchedAt  time.Time
	ChangedAt  time.Time
	ETag       string
	ExpiresAt  time.Time
	State      string
	Error      string
	RetryAfter time.Time
	// Status is what the upstream answered, when the route declared that answer
	Status int
}

// Freshness reads one key's bookkeeping. A key never fetched is (nil, nil):
// absent is an answer here, not an error.
func (s *Store) Freshness(ctx context.Context, kind, key string) (*Freshness, error) {
	row, err := s.q.GetFreshness(ctx, dbgen.GetFreshnessParams{Kind: kind, Key: key})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read freshness %s/%s: %w", kind, key, err)
	}
	return &Freshness{
		Kind:       row.Kind,
		Key:        row.Key,
		FetchedAt:  unixTime(row.FetchedAt),
		ChangedAt:  unixTime(row.ChangedAt),
		ETag:       row.Etag,
		ExpiresAt:  unixTime(row.ExpiresAt),
		State:      row.State,
		Error:      row.Error,
		RetryAfter: unixTime(row.RetryAfter),
		Status:     int(row.Status),
	}, nil
}

// RecordFetched stores the result of a fetch that answered. It clears any error
// and the backoff with it: holding a fetch off over a failure that already
// healed is the same outage twice.
func (s *Store) RecordFetched(ctx context.Context, f Freshness) error {
	err := s.q.RecordFetched(ctx, dbgen.RecordFetchedParams{
		Kind:      f.Kind,
		Key:       f.Key,
		FetchedAt: nullUnix(f.FetchedAt),
		ChangedAt: nullUnix(f.ChangedAt),
		Etag:      f.ETag,
		ExpiresAt: nullUnix(f.ExpiresAt),
		State:     f.State,
		Status:    int64(f.Status),
	})
	if err != nil {
		return fmt.Errorf("record fetch %s/%s: %w", f.Kind, f.Key, err)
	}
	return nil
}

// MarkFetching records that a fetch is under way.
func (s *Store) MarkFetching(ctx context.Context, kind, key string) error {
	if err := s.q.MarkFetching(ctx, dbgen.MarkFetchingParams{Kind: kind, Key: key}); err != nil {
		return fmt.Errorf("mark fetching %s/%s: %w", kind, key, err)
	}
	return nil
}

// MarkError records a failed fetch and the moment another one may start. A
// caller inside that window reads the stored error instead of asking a failing
// upstream again on every request.
func (s *Store) MarkError(ctx context.Context, kind, key, msg string, retryAfter time.Time) error {
	err := s.q.MarkError(ctx, dbgen.MarkErrorParams{
		Kind:       kind,
		Key:        key,
		Error:      msg,
		RetryAfter: nullUnix(retryAfter),
	})
	if err != nil {
		return fmt.Errorf("mark error %s/%s: %w", kind, key, err)
	}
	return nil
}

// DeleteFreshness forgets one key's bookkeeping and reports how many rows went.
func (s *Store) DeleteFreshness(ctx context.Context, kind, key string) (int64, error) {
	n, err := s.q.DeleteFreshness(ctx, dbgen.DeleteFreshnessParams{Kind: kind, Key: key})
	if err != nil {
		return 0, fmt.Errorf("delete freshness %s/%s: %w", kind, key, err)
	}
	return n, nil
}

// Watermark reads the newest view already applied for a subject. A subject
// nothing has been applied for reports found=false.
func (s *Store) Watermark(ctx context.Context, subject string) (time.Time, bool, error) {
	at, err := s.q.GetWatermark(ctx, subject)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read watermark %q: %w", subject, err)
	}
	return time.Unix(at, 0).UTC(), true, nil
}

// ApplyWatermark advances a subject's watermark to at and reports whether it
// applied. A view older than the stored one restates superseded state, so it is
// refused.
//
// An EQUAL time applies. The clock is a second, and two genuinely distinct
func (s *Store) ApplyWatermark(ctx context.Context, subject string, at time.Time) (bool, error) {
	n, err := s.q.ApplyWatermark(ctx, dbgen.ApplyWatermarkParams{
		Subject:   subject,
		AppliedAt: at.Unix(),
	})
	if err != nil {
		return false, fmt.Errorf("apply watermark %q: %w", subject, err)
	}
	return n > 0, nil
}

// PruneWatermarks drops subjects untouched since before, and reports how many
// went. A subject nothing has restated is a subject nothing will restate.
func (s *Store) PruneWatermarks(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.q.PruneWatermarks(ctx, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune watermarks: %w", err)
	}
	return n, nil
}

// Grant is proof that one principal read one resource key upstream.
type Grant struct {
	Principal string
	Resource  string
	Key       string
	// Source names what earned it: a probe, or a list sync.
	Source    string
	ExpiresAt time.Time
}

// HasGrant reports whether this principal holds live proof for this key. The
// query filters on expiry, so an expired grant is unusable the moment it
// expires rather than the moment a prune reaches it.
func (s *Store) HasGrant(ctx context.Context, principal, resource, key string, now time.Time) (bool, error) {
	n, err := s.q.CountLiveGrants(ctx, dbgen.CountLiveGrantsParams{
		Principal: principal,
		Resource:  resource,
		Key:       key,
		ExpiresAt: now.Unix(),
	})
	if err != nil {
		return false, fmt.Errorf("read grant %s %s/%s: %w", principal, resource, key, err)
	}
	return n > 0, nil
}

// RecordGrant stores proof and drops this principal's cached denial of the same
// key in the same transaction. Upstream just said yes; a cached no beside it
// answers one question two ways.
func (s *Store) RecordGrant(ctx context.Context, g Grant) error {
	if err := s.tx(ctx, func(q *dbgen.Queries) error { return recordGrant(ctx, q, g) }); err != nil {
		return fmt.Errorf("record grant %s %s/%s: %w", g.Principal, g.Resource, g.Key, err)
	}
	return nil
}

// ReplaceGrants replace-syncs one source's grants for a principal and resource:
// what the list no longer names is gone. Merging instead would keep proof alive
// for a key upstream stopped listing, which is access the caller lost.
func (s *Store) ReplaceGrants(ctx context.Context, principal, resource, source string, keys []string, expires time.Time) error {
	err := s.tx(ctx, func(q *dbgen.Queries) error {
		_, err := q.DeleteGrantsBySource(ctx, dbgen.DeleteGrantsBySourceParams{
			Principal: principal,
			Resource:  resource,
			Source:    source,
		})
		if err != nil {
			return err
		}
		for _, k := range keys {
			g := Grant{Principal: principal, Resource: resource, Key: k, Source: source, ExpiresAt: expires}
			if err := recordGrant(ctx, q, g); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replace grants %s %s: %w", principal, resource, err)
	}
	return nil
}

// recordGrant writes one grant and clears the denial it contradicts.
func recordGrant(ctx context.Context, q *dbgen.Queries, g Grant) error {
	err := q.RecordGrant(ctx, dbgen.RecordGrantParams{
		Principal: g.Principal,
		Resource:  g.Resource,
		Key:       g.Key,
		Source:    g.Source,
		ExpiresAt: g.ExpiresAt.Unix(),
	})
	if err != nil {
		return err
	}
	_, err = q.DeleteDenial(ctx, dbgen.DeleteDenialParams{
		Principal: g.Principal,
		Resource:  g.Resource,
		Key:       g.Key,
	})
	return err
}

// RevokeGrant drops one principal's proof for one key and reports whether there
// was any.
func (s *Store) RevokeGrant(ctx context.Context, principal, resource, key string) (bool, error) {
	n, err := s.q.RevokeGrant(ctx, dbgen.RevokeGrantParams{
		Principal: principal,
		Resource:  resource,
		Key:       key,
	})
	if err != nil {
		return false, fmt.Errorf("revoke grant %s %s/%s: %w", principal, resource, key, err)
	}
	return n > 0, nil
}

// PruneGrants deletes grants that expired at or before now, and reports how
// many went. Reads already ignore them; this reclaims the space.
func (s *Store) PruneGrants(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.PruneGrants(ctx, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune grants: %w", err)
	}
	return n, nil
}

// Denial reads a live cached refusal and the status upstream answered with. An
// expired or absent denial reports found=false, so the caller re-asks upstream.
func (s *Store) Denial(ctx context.Context, principal, resource, key string, now time.Time) (int, bool, error) {
	status, err := s.q.GetDenial(ctx, dbgen.GetDenialParams{
		Principal: principal,
		Resource:  resource,
		Key:       key,
		ExpiresAt: now.Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read denial %s %s/%s: %w", principal, resource, key, err)
	}
	return int(status), true, nil
}

// RecordDenial caches an authoritative refusal until expires. Only an
// authoritative status belongs here: caching a transient failure turns one bad
// minute upstream into a caller who cannot read what they own.
func (s *Store) RecordDenial(ctx context.Context, principal, resource, key string, status int, expires time.Time) error {
	err := s.q.RecordDenial(ctx, dbgen.RecordDenialParams{
		Principal: principal,
		Resource:  resource,
		Key:       key,
		Status:    int64(status),
		ExpiresAt: expires.Unix(),
	})
	if err != nil {
		return fmt.Errorf("record denial %s %s/%s: %w", principal, resource, key, err)
	}
	return nil
}

// PruneDenials deletes denials that expired at or before now, and reports how
// many went.
func (s *Store) PruneDenials(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.PruneDenials(ctx, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune denials: %w", err)
	}
	return n, nil
}

// nullUnix stores a time as a Unix second, and a zero time as NULL.
func nullUnix(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// unixTime reads a stored second back, and NULL as the zero time.
func unixTime(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0).UTC()
}
