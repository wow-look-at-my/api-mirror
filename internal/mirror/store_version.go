package mirror

import (
	"errors"
	"fmt"
	"strings"
)

// errStoredIsNewer is a write refused because the stored row carries a later
// version than the write does.
var errStoredIsNewer = errors.New("the stored row is newer than this write")

func versionFields(r *Resource) []Field {
	var out []Field
	for _, f := range r.Fields {
		if f.Version {
			out = append(out, f)
		}
	}
	return out
}

// versionColumn names the column that orders writes to a row, or "".
func versionColumn(r *Resource) string {
	if r.Store != StoreColumns {
		return ""
	}
	if v := versionFields(r); len(v) == 1 {
		return v[0].Name
	}
	return ""
}

// upsertStmt writes a row unless the stored a single carries a later
// version. A write or a stored row with no version is not ordered, so it
// applies; equal versions apply, because both describe the same moment.
func upsertStmt(r *Resource, version string) string {
	cols := columnsOf(r)
	table := resourceTable(r.Name)
	sets := make([]string, 0, len(cols)+1)
	for _, c := range append(cols, "mirror_written_at") {
		sets = append(sets, fmt.Sprintf("%s = excluded.%s", c, c))
	}
	return fmt.Sprintf(`INSERT INTO %s (%s, mirror_written_at) VALUES (%s)
ON CONFLICT (%s) DO UPDATE SET %s
WHERE excluded.%s IS NULL OR %s.%s IS NULL OR excluded.%s >= %s.%s`,
		table, strings.Join(cols, ", "), placeholders(len(cols)+1),
		strings.Join(keyNames(r), ", "), strings.Join(sets, ", "),
		version, table, version, version, table, version)
}
