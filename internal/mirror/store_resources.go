package mirror

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The per-resource half of the store.
//
// A resource table exists because a spec declared it, so no generator can type
// it and every statement here is assembled at run time. That makes the column
// list the one thing worth guarding: it comes from columnsOf, through the
// builders below, and from nowhere else. A statement written by hand somewhere
// in the engine is how a column gets written in one order and read in another.

// Row is one stored fact: column name to value, as read back from SQLite.
type Row map[string]any

// Put writes one row of a resource, replacing what is there.
func (s *Store) Put(ctx context.Context, r *Resource, row Row, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, insertStmt(r), rowArgs(r, row, at)...); err != nil {
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

	stmt, err := tx.PrepareContext(ctx, insertStmt(r))
	if err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, rowArgs(r, row, at)...); err != nil {
			return fmt.Errorf("store %s: %w", r.Name, err)
		}
	}
	return tx.Commit()
}

// ReplaceMany makes the rows under one partial key exactly the rows given, in
// one transaction.
//
// An upsert alone cannot express a DELETION. A list answer that no longer
// mentions an item is the upstream saying the item is gone, and a store that
// only ever adds keeps serving it for good. Both halves have to land together:
// a delete that commits without its insert empties the list instead of
// correcting it.
func (s *Store) ReplaceMany(ctx context.Context, r *Resource, key map[string]string, rows []Row, at time.Time) error {
	where, args := keyPredicate(r, key)
	if where == "" {
		return fmt.Errorf("replace %s: no key supplied, which would empty the table", r.Name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, deleteStmt(r, where), args...); err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	stmt, err := tx.PrepareContext(ctx, insertStmt(r))
	if err != nil {
		return fmt.Errorf("store %s: %w", r.Name, err)
	}
	defer stmt.Close()

	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, rowArgs(r, row, at)...); err != nil {
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

	rows, err := s.db.QueryContext(ctx, selectStmt(r, where, false), args...)
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
	where, args := keyPredicate(r, key)

	rows, err := s.db.QueryContext(ctx, selectStmt(r, where, true), args...)
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

// Delete removes the rows matching a partial key and reports how many went. It
// refuses an empty key: a spec bug that yields no key must not read as a
// request to empty the table.
func (s *Store) Delete(ctx context.Context, r *Resource, key map[string]string) (int64, error) {
	where, args := keyPredicate(r, key)
	if where == "" {
		return 0, fmt.Errorf("delete %s: refusing to delete every row", r.Name)
	}
	res, err := s.db.ExecContext(ctx, deleteStmt(r, where), args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", r.Name, err)
	}
	return res.RowsAffected()
}

// insertStmt builds the one write statement every resource write uses.
func insertStmt(r *Resource) string {
	cols := columnsOf(r)
	return fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s, mirror_written_at) VALUES (%s)`,
		resourceTable(r.Name), strings.Join(cols, ", "), placeholders(len(cols)+1))
}

// selectStmt builds the one read statement every resource read uses. An empty
// where reads the whole table; ordered adds the key order a list answer needs.
func selectStmt(r *Resource, where string, ordered bool) string {
	stmt := fmt.Sprintf(`SELECT %s FROM %s`,
		strings.Join(columnsOf(r), ", "), resourceTable(r.Name))
	if where != "" {
		stmt += " WHERE " + where
	}
	if ordered {
		stmt += " ORDER BY " + strings.Join(keyNames(r), ", ")
	}
	return stmt
}

// deleteStmt builds the one delete statement. The where is never optional here
// -- Delete refuses an empty one before it gets this far.
func deleteStmt(r *Resource, where string) string {
	return fmt.Sprintf(`DELETE FROM %s WHERE %s`, resourceTable(r.Name), where)
}

// rowArgs lays a row out in the order insertStmt names the columns, with the
// write time last. Both read columnsOf, so the two cannot disagree.
func rowArgs(r *Resource, row Row, at time.Time) []any {
	cols := columnsOf(r)
	args := make([]any, 0, len(cols)+1)
	for _, c := range cols {
		args = append(args, row[c])
	}
	return append(args, at.Unix())
}

// keyPredicate builds a WHERE clause over the columns present in key.
//
// A key column is the usual case. A FIELD is allowed too, because a list route
// often selects by an attribute rather than by identity -- every post by an
// author, where the post's identity is its own id. Only declared columns are
// used, in the resource's own order, so a caller cannot smuggle a predicate in
// and the same request always builds the same statement.
func keyPredicate(r *Resource, key map[string]string) (string, []any) {
	var terms []string
	var args []any
	add := func(name string) {
		v, ok := key[name]
		if !ok {
			return
		}
		terms = append(terms, name+" = ?")
		args = append(args, v)
	}
	for _, k := range r.Keys {
		add(k.Name)
	}
	for _, f := range r.Fields {
		add(f.Name)
	}
	return strings.Join(terms, " AND "), args
}

// keyNames returns a resource's key columns in declared order.
func keyNames(r *Resource) []string {
	out := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		out = append(out, k.Name)
	}
	return out
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
