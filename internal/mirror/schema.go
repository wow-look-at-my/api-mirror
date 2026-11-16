// schema.go holds enginePrefix.

package mirror

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/wow-look-at-my/api-mirror/internal/database"
	"github.com/wow-look-at-my/go-containers/set"
)

// The engine's own tables. They are the same for every spec.
const enginePrefix = "mirror_"

// resourceTable is the derived table name for a resource.
func resourceTable(name string) string { return "res_" + name }

// sqlType maps a declared field type to its SQLite column type.
func sqlType(t FieldType) string {
	switch t {
	case FieldInt, FieldBool, FieldTime:
		return "INTEGER"
	default:
		return "TEXT"
	}
}

// resourceDDL derives resource's table.
//
// The keys are the primary key, so the store cannot hold rows for fact
// however many callers ask for it. There is no actor column here and no way for
// a spec to add.
func resourceDDL(r *Resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", resourceTable(r.Name))
	for _, k := range r.Keys {
		fmt.Fprintf(&b, "\t%s TEXT NOT NULL,\n", k.Name)
	}
	for _, k := range r.Keys {
		if c := k.sentColumn(); c != "" {
			fmt.Fprintf(&b, "\t%s TEXT,\n", c)
		}
	}
	if held := heldColumns(r); held != nil {
		for _, c := range held {
			fmt.Fprintf(&b, "\t%s TEXT NOT NULL,\n", c)
		}
	} else {
		for _, f := range r.Fields {
			fmt.Fprintf(&b, "\t%s %s,\n", f.Name, sqlType(f.Type))
		}
	}
	// The engine's own column carries a mirror_ prefix so it cannot collide
	b.WriteString("\tmirror_written_at INTEGER NOT NULL,\n")
	names := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		names = append(names, k.Name)
	}
	fmt.Fprintf(&b, "\tPRIMARY KEY (%s)\n);\n", strings.Join(names, ", "))
	// The row cap evicts oldest which this index makes a range read.
	fmt.Fprintf(&b, "CREATE INDEX %s_written ON %s (mirror_written_at);\n", resourceTable(r.Name), resourceTable(r.Name))
	return b.String()
}

//go:embed list_member.sql
var listMemberDDL string

// DDL is the whole schema: the engine's fixed tables, then the resources in
// declared order.
func (s *Spec) DDL() string {
	var b strings.Builder
	b.WriteString(strings.TrimLeft(database.Schema, "\n"))
	b.WriteString("\n")
	b.WriteString(listMemberDDL)
	b.WriteString("\n")
	b.WriteString(refusalDDL)
	for _, r := range s.Resources {
		b.WriteString("\n")
		b.WriteString(resourceDDL(r))
	}
	return b.String()
}

// Fingerprint identifies the whole schema, static tables and derived ones
func (s *Spec) Fingerprint() string {
	return database.Fingerprint(s.DDL())
}

// columnsOf returns a resource's stored column names in a stable order: keys as
// declared, then fields as declared. Every write and read builds its statement
// from this, so a column can never be written in order and read in another.
func columnsOf(r *Resource) []string {
	out := make([]string, 0, len(r.Keys)+len(r.Fields)+1)
	for _, k := range r.Keys {
		out = append(out, k.Name)
	}
	for _, k := range r.Keys {
		if c := k.sentColumn(); c != "" {
			out = append(out, c)
		}
	}
	if held := heldColumns(r); held != nil {
		return append(out, held...)
	}
	for _, f := range r.Fields {
		out = append(out, f.Name)
	}
	return out
}

// heldColumns are the engine's own columns that hold a non-column answer.
// A column resource has none.
func heldColumns(r *Resource) []string {
	switch r.Store {
	case StoreDocument, StoreRaw:
		return []string{"document"}
	case StoreRedirect:
		return []string{colLocation}
	case StoreText:
		return []string{colBody, colMedia}
	}
	return nil
}

// The held columns of the newer store modes carry the engine's prefix, so a
// declared name can never collide with them.
const (
	colLocation = enginePrefix + "location"
	colBody     = enginePrefix + "body"
	colMedia    = enginePrefix + "media"
)

// reservedColumns are the names the engine owns inside a resource table.
var reservedColumns = set.Of("mirror_written_at", "document", "rowid")

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
		// The engine's own columns carry its prefix, so a declared name may not.
		declared := make([]string, 0, len(r.Keys)+len(r.Fields))
		for _, k := range r.Keys {
			declared = append(declared, k.Name)
		}
		for _, f := range r.Fields {
			declared = append(declared, f.Name)
		}
		for _, name := range declared {
			if strings.HasPrefix(name, enginePrefix) {
				return fmt.Errorf("resource %q: %q: the %s prefix belongs to the engine", r.Name, name, enginePrefix)
			}
		}
		for _, c := range columnsOf(r) {
			if err := validateIdent(fmt.Sprintf("resource %q column", r.Name), c); err != nil {
				return err
			}
			if reservedColumns.Contains(c) && !slices.Contains(heldColumns(r), c) {
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
	if sqlKeywords.Contains(name) {
		return fmt.Errorf("%s %q: that is a SQL keyword", what, name)
	}
	return nil
}

// sqlKeywords is the subset a field name plausibly collides with. It is not
// SQLite's full list: a name outside this set and inside validateIdent's shape
// parses unquoted.
var sqlKeywords = set.Of(
	"index", "table", "select", "from", "where",
	"order", "group", "primary", "key", "default",
	"unique", "check", "references", "constraint",
	"create", "drop", "insert", "update", "delete",
	"values", "join", "on", "as", "null",
	"not", "and", "or", "in", "is",
)
