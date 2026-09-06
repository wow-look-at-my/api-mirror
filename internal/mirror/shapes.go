package mirror

import (
	"html/template"
	"sort"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/go-containers/set"
)

// Shapes: turning "this request left" into "this family of requests is still
// leaving", which is the only form of that fact anybody can act on.

// generalize reduces a path to a shape using nothing but the path: a number or
// a long hex string is an identity, everything else is vocabulary. It is the
// fallback for a spec that declares no routes yet.
func generalize(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if looksLikeIdentity(s) {
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// generalizeWith shapes a path by the spec's vocabulary and segment position.
//
// A declared word stays. So do the and last segments unless they look
// like identities -- losing the last names nothing to model. The rest are
// identities, which is a guess, so a brief item carries real sample paths.
func generalizeWith(vocab set.Set[string], path string) string {
	if vocab.Len() == 0 {
		return generalize(path)
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if vocab.Contains(strings.ToLower(s)) {
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
func pathVocabulary(spec *Spec) set.Set[string] {
	vocab := set.New[string]()
	add := func(pattern string) {
		for _, s := range strings.Split(strings.Trim(pattern, "/"), "/") {
			if s == "" || (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
				continue
			}
			vocab.Add(strings.ToLower(s))
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

// BriefItem is family of requests the spec does not model yet, with the
// declaration that would model it.
type BriefItem struct {
	Method string `json:"method"`
	Shape  string `json:"shape"`
	Count  int    `json:"count"`
	// Reasons is why these left; reasons need different fixes.
	Reasons map[string]int `json:"reasons"`
	Bytes   int            `json:"bytes"`
	// Samples are real paths, so an author can check the guess the shape made.
	Samples []string `json:"samples,omitempty"`
	// Sketch is a starting <route>, not a finished declaration.
	Sketch string `json:"sketch"`
}

// Brief reports what is still leaving, worst, with a sketch for each.
// This is why a passthrough reason is named: "some traffic is uncached" is a
// mood, "this shape left times, here is the route" is a task.
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
		// Reporting it is cheaper than a comment claiming it cannot happen.
		return "sketch: " + err.Error()
	}
	return b.String()
}

// nameShape names each identity segment distinctly, so a sketch does not
// declare parameters all called {id}.
func nameShape(shape string) (string, []string) {
	segs := strings.Split(strings.Trim(shape, "/"), "/")
	var keys []string
	for i, s := range segs {
		if s != "{id}" && !(strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
			continue
		}
		// After the last literal before it: adjacent parameters still differ.
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

// singular trims trailing "s", so /repos/{x} names its key "repo".
func singular(s string) string {
	if len(s) > 1 && strings.HasSuffix(s, "s") {
		return s[:len(s)-1]
	}
	return s
}
