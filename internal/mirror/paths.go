package mirror

import (
	"fmt"
	"strconv"
	"time"
)

// Reading one declared field out of a decoded document.
//
// These sit beside lookupPath rather than inside it because they carry the
// engine's own reading of an absent value: absent is reported as absent, never
// as a zero. A zero written where a value was missing is the class of bug that
// stores an outage as a fact.

// stringAt reads a path as text. An absent path is the empty string, which the
// caller must treat as "not there" rather than as a value.
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

// itoa is strconv.Itoa under a shorter name, for the error strings that would
// otherwise pull strconv into a file that needs nothing else from it.
func itoa(n int) string { return strconv.Itoa(n) }
