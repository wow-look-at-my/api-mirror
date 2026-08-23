package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the derived SQLite database: engine bookkeeping plus one table per
// declared resource.
type Store struct {
	db   *sql.DB
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

	s := &Store{db: db, spec: spec, byName: make(map[string]*Resource, len(spec.Resources))}
	for _, r := range spec.Resources {
		s.byName[r.Name] = r
	}
	return s, nil
}

func openDB(path string) (*sql.DB, error) {
	// WAL keeps a reader from blocking the writer, which matters because a
	// detached fetch writes while requests read. busy_timeout turns the
	// remaining contention into a wait instead of an error.
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

func readFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='mirror_meta'`).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read schema state: %w", err)
	}
	var fp string
	err = db.QueryRowContext(ctx, `SELECT value FROM mirror_meta WHERE key='fingerprint'`).Scan(&fp)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read fingerprint: %w", err)
	}
	return fp, nil
}

func applySchema(ctx context.Context, db *sql.DB, spec *Spec, fingerprint string) error {
	if _, err := db.ExecContext(ctx, spec.DDL()); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO mirror_meta (key, value) VALUES ('fingerprint', ?)`, fingerprint)
	if err != nil {
		return fmt.Errorf("record fingerprint: %w", err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Resource resolves a declared resource by name.
func (s *Store) Resource(name string) (*Resource, bool) {
	r, ok := s.byName[name]
	return r, ok
}

// Row is one stored fact: column name to value, as read back from SQLite.
type Row map[string]any

// Put writes one row of a resource, replacing what is there.
//
// Every write goes through this one statement, built from columnsOf, so a
// column cannot be written in one order and read in another.
func (s *Store) Put(ctx context.Context, r *Resource, row Row, at time.Time) error {
	cols := columnsOf(r)
	args := make([]any, 0, len(cols)+1)
	for _, c := range cols {
		args = append(args, row[c])
	}
	args = append(args, at.Unix())

	stmt := fmt.Sprintf(
		`INSERT OR REPLACE INTO %s (%s, updated_at) VALUES (%s, ?)`,
		resourceTable(r.Name),
		strings.Join(cols, ", "),
		placeholders(len(cols)),
	)
	if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	return nil
}

// PutMany writes a whole list answer in one transaction. A partially written
// list is a list that reads as complete and is not.
func (s *Store) PutMany(ctx context.Context, r *Resource, rows []Row, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	defer tx.Rollback()

	cols := columnsOf(r)
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT OR REPLACE INTO %s (%s, updated_at) VALUES (%s, ?)`,
		resourceTable(r.Name), strings.Join(cols, ", "), placeholders(len(cols)),
	))
	if err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	defer stmt.Close()

	for _, row := range rows {
		args := make([]any, 0, len(cols)+1)
		for _, c := range cols {
			args = append(args, row[c])
		}
		args = append(args, at.Unix())
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("store %s: %w", r.Name, err)
		}
	}
	return tx.Commit()
}

// Get reads one row by its full key. A missing row is (nil, nil): absent is an
// answer here, not an error.
func (s *Store) Get(ctx context.Context, r *Resource, key map[string]string) (Row, error) {
	where, args := keyPredicate(r, key)
	if where == "" {
		return nil, fmt.Errorf("get %s: no key supplied", r.Name)
	}
	cols := columnsOf(r)
	stmt := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`,
		strings.Join(cols, ", "), resourceTable(r.Name), where)

	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.Name, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	row, err := scanRow(rows, cols)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.Name, err)
	}
	return row, rows.Err()
}

// List reads every row matching a partial key, in key order. An empty match is
// an empty list, never an error: the caller decides whether that means "no rows
// upstream" or "not fetched yet", and the freshness marker is what tells them.
func (s *Store) List(ctx context.Context, r *Resource, key map[string]string) ([]Row, error) {
	cols := columnsOf(r)
	stmt := fmt.Sprintf(`SELECT %s FROM %s`, strings.Join(cols, ", "), resourceTable(r.Name))
	where, args := keyPredicate(r, key)
	if where != "" {
		stmt += " WHERE " + where
	}
	names := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		names = append(names, k.Name)
	}
	stmt += " ORDER BY " + strings.Join(names, ", ")

	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.Name, err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		row, err := scanRow(rows, cols)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", r.Name, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete removes the rows matching a partial key and reports how many went.
func (s *Store) Delete(ctx context.Context, r *Resource, key map[string]string) (int64, error) {
	where, args := keyPredicate(r, key)
	if where == "" {
		return 0, fmt.Errorf("delete %s: refusing to delete every row", r.Name)
	}
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE %s`, resourceTable(r.Name), where), args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", r.Name, err)
	}
	return res.RowsAffected()
}

// keyPredicate builds a WHERE clause over the key columns present in key. Only
// declared key columns are used, so a caller cannot smuggle a predicate in.
func keyPredicate(r *Resource, key map[string]string) (string, []any) {
	var terms []string
	var args []any
	for _, k := range r.Keys {
		v, ok := key[k.Name]
		if !ok {
			continue
		}
		terms = append(terms, k.Name+" = ?")
		args = append(args, v)
	}
	return strings.Join(terms, " AND "), args
}

func scanRow(rows *sql.Rows, cols []string) (Row, error) {
	cells := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(Row, len(cols))
	for i, c := range cols {
		out[c] = cells[i]
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
