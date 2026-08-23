package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// absorb projects one upstream document onto a resource's declared columns.
//
// A field the spec does not declare is a field the mirror does not keep, and so
// a field it never serves. That is the whole point of rebuilding rather than
// replaying: the answer's shape is the spec's, and it cannot drift with
// whatever the upstream added this week.
func absorb(r *Resource, doc any) (Row, error) {
	row := make(Row, len(r.Keys)+len(r.Fields))
	for _, k := range r.Keys {
		if k.From == "" {
			continue
		}
		v := lookupPath(doc, k.From)
		if v == nil {
			continue
		}
		row[k.Name] = foldKey(k, fmt.Sprintf("%v", v))
	}
	for _, f := range r.Fields {
		v, err := fieldValue(f, doc)
		if err != nil {
			return nil, fmt.Errorf("absorb %s.%s: %w", r.Name, f.Name, err)
		}
		row[f.Name] = v
	}
	return row, nil
}

// foldKey applies a key component's declared case folding.
func foldKey(k Key, v string) string {
	if k.Fold {
		return strings.ToLower(v)
	}
	return v
}

// fieldValue reads one declared field out of a document and coerces it to the
// column's type. A value the type cannot hold is an error, not a zero: a zero
// written here is indistinguishable from a real zero upstream.
func fieldValue(f Field, doc any) (any, error) {
	var raw any
	if f.Expr != "" {
		s, err := renderString(f.Expr, doc)
		if err != nil {
			return nil, err
		}
		raw = s
	} else {
		raw = lookupPath(doc, f.From)
	}
	return coerce(f.Type, raw)
}

// coerce turns a decoded JSON value into what its declared column stores. A nil
// stays nil: absent is a value, and the caller decides whether to write it.
func coerce(t FieldType, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t {
	case FieldText:
		switch x := v.(type) {
		case string:
			return x, nil
		case json.Number:
			return x.String(), nil
		default:
			return fmt.Sprintf("%v", x), nil
		}
	case FieldInt:
		return toInt(v)
	case FieldBool:
		switch x := v.(type) {
		case bool:
			return boolToInt(x), nil
		case string:
			return boolToInt(isTruthy(x)), nil
		default:
			return nil, fmt.Errorf("%v is not a bool", v)
		}
	case FieldTime:
		return toUnix(v)
	case FieldJSON:
		b, err := marshalJSON(v)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	default:
		return nil, fmt.Errorf("unknown field type %q", t)
	}
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func toInt(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return nil, fmt.Errorf("%s is not an integer", x.String())
		}
		return n, nil
	case float64:
		return int64(x), nil
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case bool:
		return boolToInt(x), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", x)
		}
		return n, nil
	default:
		return nil, fmt.Errorf("%v is not an integer", v)
	}
}

// toUnix reads a time as a Unix second. Both encodings an API plausibly uses
// are accepted -- an RFC 3339 string and a numeric epoch -- because one upstream
// uses both, sometimes for the same field, and a reader that handles only one
// silently loses every value in the other shape.
func toUnix(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return nil, fmt.Errorf("%s is not a time", x.String())
		}
		return n, nil
	case float64:
		return int64(x), nil
	case int64:
		return x, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a time", x)
		}
		return t.Unix(), nil
	default:
		return nil, fmt.Errorf("%v is not a time", v)
	}
}

// rebuild renders a stored row back into the document a consumer receives.
//
// Hit and miss both go through here, so a route's answer cannot change shape
// with cache state -- the class of bug where a consumer works until the cache
// warms up.
func rebuild(r *Resource, row Row) (map[string]any, error) {
	if r.Store == StoreDocument {
		return nil, fmt.Errorf("rebuild %s: a document resource replays its stored document", r.Name)
	}
	out := make(map[string]any, len(r.Keys)+len(r.Fields))
	for _, k := range r.Keys {
		if v, ok := row[k.Name]; ok {
			out[k.Name] = v
		}
	}
	for _, f := range r.Fields {
		v, ok := row[f.Name]
		if !ok {
			continue
		}
		out[f.Name] = present(f, v)
	}
	return out, nil
}

// present turns a stored cell back into its JSON shape.
func present(f Field, v any) any {
	if v == nil {
		return nil
	}
	switch f.Type {
	case FieldBool:
		n, err := toInt(v)
		if err != nil {
			return v
		}
		return n.(int64) != 0
	case FieldTime:
		n, err := toInt(v)
		if err != nil {
			return v
		}
		return time.Unix(n.(int64), 0).UTC().Format(time.RFC3339)
	case FieldJSON:
		s, ok := v.(string)
		if !ok {
			return v
		}
		var out any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			return s
		}
		return out
	default:
		return v
	}
}

// trim removes the declared key patterns from a document, recursively, before
// it is stored.
//
// This is what keeps a mirrored answer from pointing back at the upstream. A
// consumer handed an upstream URL follows it, and every request that leaves by
// that door is one the mirror did not cache, did not gate, and cannot see.
func trim(r *Resource, v any) any {
	if len(r.Drop) == 0 {
		return v
	}
	keep := make(map[string]bool, len(r.Keep))
	for _, k := range r.Keep {
		keep[strings.ToLower(k.Name)] = true
	}
	return trimValue(v, r.Drop, keep)
}

func trimValue(v any, drop []string, keep map[string]bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if dropped(k, drop) && !keep[strings.ToLower(k)] {
				continue
			}
			out[k] = trimValue(val, drop, keep)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = trimValue(val, drop, keep)
		}
		return out
	default:
		return v
	}
}

// dropped reports whether a key matches a drop pattern: an exact name, or a
// "*suffix" form.
func dropped(key string, drop []string) bool {
	lower := strings.ToLower(key)
	for _, p := range drop {
		p = strings.ToLower(p)
		if strings.HasPrefix(p, "*") {
			if strings.HasSuffix(lower, p[1:]) {
				return true
			}
			continue
		}
		if lower == p {
			return true
		}
	}
	return false
}

// marshalJSON renders a document the way every writer here renders it, so bytes
// stored by a fetch, bytes rewritten by a delivery, and bytes served on a hit
// are the same bytes. HTML escaping is off because an upstream does not escape,
// and a name containing "&" must survive the round trip unchanged.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeJSON decodes a body with numbers left as json.Number, so a large
// integer id survives instead of becoming a float that rounds.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
