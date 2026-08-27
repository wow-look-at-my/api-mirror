package mirror

import (
	"html/template"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Shapes: turning "this request left" into "this family of requests is still
// leaving", which is the only form of that fact anybody can act on.

// generalize reduces a concrete path to a shape using nothing but the path.
//
// It is the fallback for a mirror whose spec declares no routes yet. A segment
// that is a number, or a long hex string, is an identity; everything else is
// taken as vocabulary. The guess is stated rather than hidden, because a wrong
// shape groups two unrelated families into one row.
func generalize(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if looksLikeIdentity(s) {
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// generalizeWith reduces a path to a shape using the spec's own vocabulary and
// the position of each segment.
//
// Three rules, in order. A word some declared route uses is a word this API
// speaks, so it stays. The FIRST and LAST segments stay unless they look like
// an identity: the first names the API's top-level noun and the last names the
// collection being asked for, and losing the last one turns
// "/repos/{id}/{id}/releases" into "/repos/{id}/{id}/{id}", which tells an
// author nothing about what to model. Everything between them is an identity.
//
// The middle rule is the guess, and it is wrong for an undeclared collection
// sitting mid-path. That is why a brief item carries real sample paths beside
// its shape.
func generalizeWith(vocab []string, path string) string {
	if len(vocab) == 0 {
		return generalize(path)
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if slices.Contains(vocab, strings.ToLower(s)) {
			continue
		}
		if looksLikeIdentity(s) {
			segs[i] = "{id}"
			continue
		}
		if i == 0 || i == len(segs)-1 {
			continue
		}
		segs[i] = "{id}"
	}
	return "/" + strings.Join(segs, "/")
}

// pathVocabulary collects every literal path segment the spec declares.
func pathVocabulary(spec *Spec) []string {
	var vocab []string
	add := func(pattern string) {
		for _, s := range strings.Split(strings.Trim(pattern, "/"), "/") {
			if s == "" || (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
				continue
			}
			if word := strings.ToLower(s); !slices.Contains(vocab, word) {
				vocab = append(vocab, word)
			}
		}
	}
	for _, rt := range spec.Routes {
		add(rt.Path)
	}
	for _, p := range spec.Purges {
		add(p.Path)
	}
	return vocab
}

func looksLikeIdentity(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	if len(s) >= 7 && isHex(s) {
		return true
	}
	return false
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// BriefItem is one family of requests the spec does not model yet, with the
// declaration that would model it.
type BriefItem struct {
	Method string `json:"method"`
	Shape  string `json:"shape"`
	Count  int    `json:"count"`
	// Reasons is why these left, tallied. A shape that leaves for two different
	// reasons needs two different fixes.
	Reasons map[string]int `json:"reasons"`
	Bytes   int            `json:"bytes"`
	// Samples are real paths this shape stood for, so an author can check the
	// guess the shape made.
	Samples []string `json:"samples,omitempty"`
	// Sketch is the <route> element that would stop this leaving. It is a
	// starting point, not a finished declaration: the engine knows the path and
	// the method, and knows nothing about what the answer contains.
	Sketch string `json:"sketch"`
}

// Brief reports what is still leaving the mirror, worst first, with a
// declaration sketch for each.
//
// This is the whole point of naming a passthrough reason. "Some traffic is
// uncached" is a mood; "GET /repos/{id}/{id}/releases left 412 times because no
// route declares it, and here is the route element" is a task.
func (e *Engine) Brief() []BriefItem {
	groups := e.tel.Requests.Groups()
	out := make([]BriefItem, 0, len(groups))
	for _, g := range groups {
		if g.Dispositions[DispPassthrough] == 0 {
			continue
		}
		item := BriefItem{
			Method:  g.Method,
			Shape:   g.Shape,
			Count:   g.Dispositions[DispPassthrough],
			Reasons: g.Reasons,
			Bytes:   g.Bytes,
			Samples: g.Samples,
		}
		item.Sketch = sketchRoute(g.Method, g.Shape)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// sketchTemplate is the declaration an author starts from. It is a template
// because it is a DOCUMENT: assembling XML with successive writes is how an
// unescaped value ends up reshaping the element it was meant to sit inside.
var sketchTemplate = template.Must(template.New("sketch").Parse(
	`<resource name="TODO" ttl="1h">
{{- range .Keys }}
	<key name="{{ . }}" from="TODO"/>
{{- end }}
	<field name="TODO" type="text">TODO</field>
	<reveal>
		<probe path="TODO"/>
		<grant ttl="24h"/>
		<deny ttl="5m"/>
	</reveal>
</resource>
<route method="{{ .Method }}" path="{{ .Path }}" resource="TODO"/>`))

// sketchRoute writes the declaration an author would start from.
func sketchRoute(method, shape string) string {
	named, keys := nameShape(shape)
	var b strings.Builder
	err := sketchTemplate.Execute(&b, struct {
		Method string
		Path   string
		Keys   []string
	}{Method: method, Path: named, Keys: keys})
	if err != nil {
		// The template is a constant and its data is three strings, so this
		// cannot fail on any input the caller can supply. Reporting it is
		// cheaper than a comment claiming it never happens.
		return "sketch: " + err.Error()
	}
	return b.String()
}

// nameShape gives each identity segment a distinct name, so the sketch does not
// declare three parameters all called {id}.
func nameShape(shape string) (string, []string) {
	segs := strings.Split(strings.Trim(shape, "/"), "/")
	var keys []string
	for i, s := range segs {
		if s != "{id}" && !(strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
			continue
		}
		// Name a parameter after the last literal before it, so a path whose
		// parameters run together still gets distinct names.
		name := "id"
		for back := i - 1; back >= 0; back-- {
			if prev := segs[back]; prev != "" && !strings.HasPrefix(prev, "{") {
				name = singular(prev)
				break
			}
		}
		for n := 2; containsString(keys, name); n++ {
			name = strings.TrimRight(name, "0123456789") + itoa(n)
		}
		keys = append(keys, name)
		segs[i] = "{" + name + "}"
	}
	return "/" + strings.Join(segs, "/"), keys
}

// singular trims one trailing "s", so /repos/{x} names its key "repo".
func singular(s string) string {
	if len(s) > 1 && strings.HasSuffix(s, "s") {
		return s[:len(s)-1]
	}
	return s
}
