package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
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

// engineDDL is the fixed part of the schema: freshness bookkeeping, the
// ordering watermark, and the reveal layer's grants and denials.
//
// Grants and denials are the ONLY tables keyed by a principal. That asymmetry
// is the whole security model: what is stored is global, what is proven is
// per-caller.
const engineDDL = `
CREATE TABLE mirror_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE mirror_freshness (
	kind        TEXT NOT NULL,
	key         TEXT NOT NULL,
	fetched_at  INTEGER,
	changed_at  INTEGER,
	etag        TEXT NOT NULL DEFAULT '',
	expires_at  INTEGER,
	state       TEXT NOT NULL DEFAULT 'unknown',
	error       TEXT NOT NULL DEFAULT '',
	retry_after INTEGER,
	PRIMARY KEY (kind, key)
);

CREATE TABLE mirror_watermark (
	subject    TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
);

CREATE TABLE mirror_grant (
	principal  TEXT NOT NULL,
	resource   TEXT NOT NULL,
	key        TEXT NOT NULL,
	source     TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (principal, resource, key)
);
CREATE INDEX mirror_grant_expiry ON mirror_grant (expires_at);

CREATE TABLE mirror_deny (
	principal  TEXT NOT NULL,
	resource   TEXT NOT NULL,
	key        TEXT NOT NULL,
	status     INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (principal, resource, key)
);
CREATE INDEX mirror_deny_expiry ON mirror_deny (expires_at);
`

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

// DDL is the whole derived schema, engine tables first, resources in declared
// order.
func (s *Spec) DDL() string {
	var b strings.Builder
	b.WriteString(strings.TrimLeft(engineDDL, "\n"))
	for _, r := range s.Resources {
		b.WriteString("\n")
		b.WriteString(resourceDDL(r))
	}
	return b.String()
}

// Fingerprint identifies the derived schema. A database recording a different
// one is nuked and recreated on open.
//
// The number is computed from the DDL the spec derives, so it cannot be
// forgotten: editing a resource changes the tables, which changes this, which
// nukes. A hand-maintained version constant is the thing this replaces, because
// a hand-maintained one gets deployed against the tables it no longer describes.
//
// A cache is disposable by definition. Nuking costs a refetch, not data.
func (s *Spec) Fingerprint() string {
	sum := sha256.Sum256([]byte(normalizeDDL(s.DDL())))
	return hex.EncodeToString(sum[:])
}

// normalizeDDL collapses whitespace so reindenting the generator does not nuke
// every deployment. Only the shape of the tables counts.
func normalizeDDL(ddl string) string {
	return strings.Join(strings.Fields(ddl), " ")
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
var reservedColumns = map[string]bool{
	"updated_at": true,
	"document":   true,
	"rowid":      true,
}

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
			if reservedColumns[c] && !(c == "document" && r.Store == StoreDocument) {
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
	if sqlKeywords[name] {
		return fmt.Errorf("%s %q: that is a SQL keyword", what, name)
	}
	return nil
}

// sqlKeywords is the subset a field name plausibly collides with. It is not
// SQLite's full list: a name outside this set and inside validateIdent's shape
// parses unquoted.
var sqlKeywords = map[string]bool{
	"index": true, "table": true, "select": true, "from": true, "where": true,
	"order": true, "group": true, "primary": true, "key": true, "default": true,
	"unique": true, "check": true, "references": true, "constraint": true,
	"create": true, "drop": true, "insert": true, "update": true, "delete": true,
	"values": true, "join": true, "on": true, "as": true, "null": true,
	"not": true, "and": true, "or": true, "in": true, "is": true,
}

// sortedKeys returns a map's keys in a stable order, so a derived statement is
// the same on every boot.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
