package mirror

import (
	"fmt"
	"strconv"
	"time"
)

// Reading one declared field out of a decoded document. These sit beside
// lookupPath because they carry the engine's reading of an absent value:
// absent, never zero. A zero written for a missing value stores an outage as
// a fact.

// stringAt reads a path as text. Empty means "not there", never a value.
func stringAt(doc any, path string) string {
	if path == "" {
		return ""
	}
	switch v := lookupPath(doc, path).(type) {
	case nil:
		return ""
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// timeAt reads a path as a moment, and reports whether it was there at all.
func timeAt(doc any, path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	raw := lookupPath(doc, path)
	if raw == nil {
		return time.Time{}, false
	}
	secs, err := toUnix(raw)
	if err != nil {
		return time.Time{}, false
	}
	n, ok := secs.(int64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// itoa keeps strconv out of files that need nothing else from it.
func itoa(n int) string { return strconv.Itoa(n) }
