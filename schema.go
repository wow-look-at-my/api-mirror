package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wow-look-at-my/api-mirror/internal/database"
)

// The engine's own tables. They are the same for every spec, so they carry a
// mirror_ prefix and a resource may not take one.
const enginePrefix = "mirror_"

// resourceTable is the derived table name for a resource.
func resourceTable(name string) string { return "res_" + name }

// sqlType maps a declared field type to its SQLite column type. A time is
// stored as a Unix second so ordering is arithmetic, never string comparison of
// whatever format the upstream chose that day.
func sqlType(t FieldType) string {
	switch t {
	case FieldInt, FieldBool, FieldTime:
		return "INTEGER"
	default:
		return "TEXT"
	}
}

// resourceDDL derives one resource's table.
//
// The keys are the primary key, so the store cannot hold two rows for one fact
// however many callers ask for it. There is no actor column here and no way for
// a spec to add one.
func resourceDDL(r *Resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", resourceTable(r.Name))
	for _, k := range r.Keys {
		fmt.Fprintf(&b, "\t%s TEXT NOT NULL,\n", k.Name)
	}
	switch r.Store {
	case StoreDocument:
		b.WriteString("\tdocument TEXT NOT NULL,\n")
	default:
		for _, f := range r.Fields {
			fmt.Fprintf(&b, "\t%s %s,\n", f.Name, sqlType(f.Type))
		}
	}
	b.WriteString("\tupdated_at INTEGER NOT NULL,\n")
	names := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		names = append(names, k.Name)
	}
	fmt.Fprintf(&b, "\tPRIMARY KEY (%s)\n);\n", strings.Join(names, ", "))
	return b.String()
}

// DDL is the whole schema: the engine's fixed tables, then the resources in
// declared order.
func (s *Spec) DDL() string {
	var b strings.Builder
	b.WriteString(strings.TrimLeft(database.Schema, "\n"))
	for _, r := range s.Resources {
		b.WriteString("\n")
		b.WriteString(resourceDDL(r))
	}
	return b.String()
}

// Fingerprint identifies the whole schema, static tables and derived ones
// alike. A database recording a different one is nuked and recreated on open.
//
// The number is computed from the DDL, so it cannot be forgotten: editing a
// resource changes the tables, which changes this, which nukes. It replaces a
// hand-maintained version constant, because a hand-maintained one gets deployed
// against the tables it no longer describes.
//
// A cache is disposable by definition. Nuking costs a refetch, not data.
func (s *Spec) Fingerprint() string {
	return database.Fingerprint(s.DDL())
}

// columnsOf returns a resource's stored column names in a stable order: keys as
// declared, then fields as declared. Every write and read builds its statement
// from this, so a column can never be written in one order and read in another.
func columnsOf(r *Resource) []string {
	out := make([]string, 0, len(r.Keys)+len(r.Fields)+1)
	for _, k := range r.Keys {
		out = append(out, k.Name)
	}
	if r.Store == StoreDocument {
		return append(out, "document")
	}
	for _, f := range r.Fields {
		out = append(out, f.Name)
	}
	return out
}

// reservedColumns are the names the engine owns inside a resource table.
var reservedColumns = []string{"updated_at", "document", "rowid"}

// validateNames rejects a spec whose names would collide with the engine's own,
// or with SQL. It runs before any DDL is derived, because a collision surfaces
// otherwise as a confusing SQL error at boot.
func (s *Spec) validateNames() error {
	for _, r := range s.Resources {
		if err := validateIdent("resource name", r.Name); err != nil {
			return err
		}
		if strings.HasPrefix(r.Name, enginePrefix) {
			return fmt.Errorf("resource %q: the %s prefix belongs to the engine", r.Name, enginePrefix)
		}
		for _, c := range columnsOf(r) {
			if err := validateIdent(fmt.Sprintf("resource %q column", r.Name), c); err != nil {
				return err
			}
			if slices.Contains(reservedColumns, c) && !(c == "document" && r.Store == StoreDocument) {
				return fmt.Errorf("resource %q: column %q is the engine's", r.Name, c)
			}
		}
	}
	return nil
}

// validateIdent allows the identifier shape that needs no SQL quoting: lower
// letters, digits and underscore, starting with a letter. Refusing everything
// else means the engine never has to guess how to quote a name.
func validateIdent(what, name string) error {
	if name == "" {
		return fmt.Errorf("%s: empty", what)
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("%s %q: must start with a lower-case letter", what, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			return fmt.Errorf("%s %q: only lower-case letters, digits and underscore are allowed", what, name)
		}
	}
	if slices.Contains(sqlKeywords, name) {
		return fmt.Errorf("%s %q: that is a SQL keyword", what, name)
	}
	return nil
}

// sqlKeywords is the subset a field name plausibly collides with. It is not
// SQLite's full list: a name outside this set and inside validateIdent's shape
// parses unquoted.
var sqlKeywords = []string{
	"index", "table", "select", "from", "where",
	"order", "group", "primary", "key", "default",
	"unique", "check", "references", "constraint",
	"create", "drop", "insert", "update", "delete",
	"values", "join", "on", "as", "null",
	"not", "and", "or", "in", "is",
}
