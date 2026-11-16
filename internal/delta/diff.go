// diff.go holds Kind.

package delta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/wow-look-at-my/go-containers/set"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind names one class of difference. An ignore rule selects by it.
type Kind string

const (
	KindStatus  Kind = "status"
	KindHeader  Kind = "header"
	KindMissing Kind = "missing"
	KindExtra   Kind = "extra"
	KindChanged Kind = "changed"
	KindType    Kind = "type"
	KindBody    Kind = "body"
	KindError   Kind = "error"
)

var allKinds = []Kind{KindStatus, KindHeader, KindMissing, KindExtra, KindChanged, KindType, KindBody, KindError}

// jsonKinds are the kinds that carry a JSONPath location.
var jsonKinds = set.Of[Kind](KindMissing, KindExtra, KindChanged, KindType)

func (k Kind) isJSON() bool {
	for j := range jsonKinds.All() {
		if k == j {
			return true
		}
	}
	return false
}

// Diff is one difference between the truth answer and a mirror answer.
type Diff struct {
	Kind Kind `json:"kind"`
	// Path is a JSONPath for a JSON kind, a header name for a header, and a byte offset for a body.
	Path   string          `json:"path,omitempty"`
	Truth  json.RawMessage `json:"truth,omitempty"`
	Mirror json.RawMessage `json:"mirror,omitempty"`
	// Truncated says Truth or Mirror is a prefix of a longer value.
	Truncated bool `json:"truncated,omitempty"`
	// Rule is the index of the ignore rule that matched, on an ignored diff.
	Rule *int `json:"rule,omitempty"`

	loc Location
}

// Shape is the path the summary groups by: the JSONPath with every index as
// [*], or Path unchanged for a kind that carries none.
func (d Diff) Shape() string {
	if d.Kind.isJSON() {
		return d.loc.Shape()
	}
	if d.Kind == KindBody {
		return ""
	}
	return d.Path
}

// Response is one target's answer to one request.
type Response struct {
	Status  int
	Header  http.Header
	Body    []byte
	Err     error
	Elapsed time.Duration
}

// maxValue bounds one rendered value in a Diff. A whole missing subtree can be megabytes, and a record is one log line.
const maxValue = 1024

// compareOpts holds what a comparison needs beyond both answers.
type compareOpts struct {
	headers    []string
	head       bool
	truthBase  string
	mirrorBase string
}

func compareResponses(truth, mirror *Response, o compareOpts) []Diff {
	if truth.Err != nil || mirror.Err != nil {
		return []Diff{{Kind: KindError, Truth: errValue(truth.Err), Mirror: errValue(mirror.Err)}}
	}
	var out []Diff
	if truth.Status != mirror.Status {
		out = append(out, Diff{Kind: KindStatus, Truth: mustJSON(truth.Status), Mirror: mustJSON(mirror.Status)})
	}
	for _, h := range o.headers {
		tv := normalizeString(strings.Join(truth.Header.Values(h), ", "), o.truthBase)
		mv := normalizeString(strings.Join(mirror.Header.Values(h), ", "), o.mirrorBase)
		if tv != mv {
			out = append(out, Diff{Kind: KindHeader, Path: http.CanonicalHeaderKey(h), Truth: mustJSON(tv), Mirror: mustJSON(mv)})
		}
	}
	if o.head {
		return out
	}
	return append(out, compareBodies(truth.Body, mirror.Body, o)...)
}

func errValue(err error) json.RawMessage {
	if err == nil {
		return nil
	}
	return mustJSON(err.Error())
}

func compareBodies(truth, mirror []byte, o compareOpts) []Diff {
	tv, tok := decodeJSON(truth)
	mv, mok := decodeJSON(mirror)
	if tok && mok {
		var out []Diff
		diffValues(normalizeValue(tv, o.truthBase), normalizeValue(mv, o.mirrorBase), nil, &out)
		return out
	}
	t := []byte(normalizeString(string(truth), o.truthBase))
	m := []byte(normalizeString(string(mirror), o.mirrorBase))
	if bytes.Equal(t, m) {
		return nil
	}
	at := 0
	for at < len(t) && at < len(m) && t[at] == m[at] {
		at++
	}
	start := max(0, at-32)
	tr, tcut := snippet(t[start:])
	mr, mcut := snippet(m[start:])
	return []Diff{{Kind: KindBody, Path: "byte " + strconv.Itoa(at), Truth: tr, Mirror: mr, Truncated: tcut || mcut}}
}

func snippet(b []byte) (json.RawMessage, bool) {
	if len(b) > maxValue {
		return mustJSON(string(b[:maxValue])), true
	}
	return mustJSON(string(b)), false
}

// decodeJSON parses a whole body as one JSON value. Numbers stay exact.
func decodeJSON(b []byte) (any, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil, false
	}
	return v, true
}

// baseToken replaces a target's own base URL in its answer.
const baseToken = "{base}"

func normalizeString(s, base string) string {
	if base == "" {
		return s
	}
	return strings.ReplaceAll(s, base, baseToken)
}

func normalizeValue(v any, base string) any {
	switch t := v.(type) {
	case string:
		return normalizeString(t, base)
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeValue(e, base)
		}
	case []any:
		for i, e := range t {
			t[i] = normalizeValue(e, base)
		}
	}
	return v
}

func diffValues(t, m any, loc Location, out *[]Diff) {
	if typeName(t) != typeName(m) {
		*out = append(*out, valueDiff(KindType, loc, t, m))
		return
	}
	switch tv := t.(type) {
	case map[string]any:
		mv := m.(map[string]any)
		keys := make([]string, 0, len(tv)+len(mv))
		for k := range tv {
			keys = append(keys, k)
		}
		for k := range mv {
			if _, ok := tv[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			te, inTruth := tv[k]
			me, inMirror := mv[k]
			child := loc.child(keySeg(k))
			switch {
			case !inMirror:
				*out = append(*out, valueDiff(KindMissing, child, te, nil))
			case !inTruth:
				*out = append(*out, valueDiff(KindExtra, child, nil, me))
			default:
				diffValues(te, me, child, out)
			}
		}
	case []any:
		mv := m.([]any)
		for i := 0; i < max(len(tv), len(mv)); i++ {
			child := loc.child(indexSeg(i))
			switch {
			case i >= len(mv):
				*out = append(*out, valueDiff(KindMissing, child, tv[i], nil))
			case i >= len(tv):
				*out = append(*out, valueDiff(KindExtra, child, nil, mv[i]))
			default:
				diffValues(tv[i], mv[i], child, out)
			}
		}
	case json.Number:
		if !numbersEqual(tv, m.(json.Number)) {
			*out = append(*out, valueDiff(KindChanged, loc, t, m))
		}
	default:
		if t != m {
			*out = append(*out, valueDiff(KindChanged, loc, t, m))
		}
	}
}

func numbersEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	af, aerr := a.Float64()
	bf, berr := b.Float64()
	return aerr == nil && berr == nil && af == bf
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func valueDiff(k Kind, loc Location, t, m any) Diff {
	d := Diff{Kind: k, Path: loc.String(), loc: loc}
	var tcut, mcut bool
	if k != KindExtra {
		d.Truth, tcut = render(t)
	}
	if k != KindMissing {
		d.Mirror, mcut = render(m)
	}
	d.Truncated = tcut || mcut
	return d
}

// render marshals a value, and replaces one over maxValue with a string of
// its first maxValue bytes.
func render(v any) (json.RawMessage, bool) {
	b := mustJSON(v)
	if len(b) <= maxValue {
		return b, false
	}
	return mustJSON(string(b[:maxValue])), true
}

// mustJSON marshals without HTML escaping, so a Link header reads as <...>
// in a log line.
func mustJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("marshal %T: %v", v, err))
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
