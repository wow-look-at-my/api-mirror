// jsonpath.go holds Seg.

package delta

import (
	"fmt"
	"strconv"
	"strings"
)

// Seg is one step of a concrete location in a JSON document: an object
// member when Index is negative, else an array element.
type Seg struct {
	Key   string
	Index int
}

func keySeg(k string) Seg { return Seg{Key: k, Index: -1} }
func indexSeg(i int) Seg  { return Seg{Index: i} }

// Location is the concrete path of one node, root first.
type Location []Seg

func (l Location) child(s Seg) Location {
	out := make(Location, len(l), len(l)+1)
	copy(out, l)
	return append(out, s)
}

func (l Location) String() string { return l.render(false) }

// Shape renders the location with every index as [*], so one difference in every element of a list groups into one row.
func (l Location) Shape() string { return l.render(true) }

func (l Location) render(shape bool) string {
	var b strings.Builder
	b.WriteByte('$')
	for _, s := range l {
		switch {
		case s.Index >= 0 && shape:
			b.WriteString("[*]")
		case s.Index >= 0:
			fmt.Fprintf(&b, "[%d]", s.Index)
		case isIdent(s.Key):
			b.WriteByte('.')
			b.WriteString(s.Key)
		default:
			b.WriteString("['")
			b.WriteString(strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s.Key))
			b.WriteString("']")
		}
	}
	return b.String()
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9'
		if !ok {
			return false
		}
	}
	return true
}

// step is one selector of a Pattern.
type step struct {
	// descend is the JSONPath ".." operator: the selector may match at any depth below the step.
	descend bool
	any     bool
	index   int
	// name is a member name. A '*' in it matches any run of characters.
	name string
}

func (s step) matches(seg Seg) bool {
	switch {
	case s.any:
		return true
	case s.index >= 0:
		return seg.Index == s.index
	default:
		return seg.Index < 0 && globMatch(s.name, seg.Key)
	}
}

// Pattern is a JSONPath subset that selects nodes by location. It supports $,
// .name, ['name'], [n], .*, [*] and the. descent.
type Pattern struct {
	src   string
	steps []step
}

func (p *Pattern) String() string { return p.src }

// Covers reports whether the pattern selects loc or an ancestor of loc.
func (p *Pattern) Covers(loc Location) bool { return coversFrom(p.steps, loc) }

func coversFrom(steps []step, loc Location) bool {
	if len(steps) == 0 {
		return true
	}
	s := steps[0]
	if !s.descend {
		return len(loc) > 0 && s.matches(loc[0]) && coversFrom(steps[1:], loc[1:])
	}
	for i := range loc {
		if s.matches(loc[i]) && coversFrom(steps[1:], loc[i+1:]) {
			return true
		}
	}
	return false
}

// ParsePattern parses a JSONPath pattern. A filter expression, a slice and a
// union are errors, never a silent non-match.
func ParsePattern(src string) (*Pattern, error) {
	if !strings.HasPrefix(src, "$") {
		return nil, fmt.Errorf("jsonpath %q: must start with $", src)
	}
	p := &Pattern{src: src}
	rest := src[1:]
	for rest != "" {
		descend := false
		switch {
		case strings.HasPrefix(rest, ".."):
			descend = true
			rest = rest[2:]
		case rest[0] == '.':
			rest = rest[1:]
		case rest[0] == '[':
		default:
			return nil, fmt.Errorf("jsonpath %q: unexpected %q", src, rest)
		}
		var (
			st  step
			err error
		)
		if rest != "" && rest[0] == '[' {
			st, rest, err = parseBracket(rest)
			if err != nil {
				return nil, fmt.Errorf("jsonpath %q: %w", src, err)
			}
		} else {
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			rest = rest[end:]
			if name == "" {
				return nil, fmt.Errorf("jsonpath %q: empty member name", src)
			}
			st = step{index: -1, name: name, any: name == "*"}
		}
		st.descend = descend
		p.steps = append(p.steps, st)
	}
	return p, nil
}

func parseBracket(s string) (step, string, error) {
	body := s[1:]
	if body != "" && (body[0] == '\'' || body[0] == '"') {
		name, n, err := unquote(body)
		if err != nil {
			return step{}, "", err
		}
		if n >= len(body) || body[n] != ']' {
			return step{}, "", fmt.Errorf("missing ] after %q", name)
		}
		return step{index: -1, name: escapeGlob(name)}, body[n+1:], nil
	}
	end := strings.IndexByte(body, ']')
	if end < 0 {
		return step{}, "", fmt.Errorf("missing ]")
	}
	inner := body[:end]
	if inner == "*" {
		return step{index: -1, any: true}, body[end+1:], nil
	}
	i, err := strconv.Atoi(inner)
	if err != nil || i < 0 {
		return step{}, "", fmt.Errorf("unsupported selector [%s]: only [n], [*] and ['name'] are supported", inner)
	}
	return step{index: i}, body[end+1:], nil
}

// unquote reads a quoted name at the start of s and returns it with the
// number of bytes it used.
func unquote(s string) (string, int, error) {
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", 0, fmt.Errorf("unterminated escape")
			}
			i++
			b.WriteByte(s[i])
		case q:
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, fmt.Errorf("unterminated quoted name")
}

// globEscape marks a literal '*' inside a quoted name. The glob matcher reads it back as a plain character.
const globEscape = "\x00"

func escapeGlob(name string) string { return strings.ReplaceAll(name, "*", globEscape+"*") }

// globMatch matches s against pattern, where '*' matches any run of
// characters and an escaped '*' matches only itself.
func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], globEscape+"*"):
			cur.WriteByte('*')
			i++
		case pattern[i] == '*':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(pattern[i])
		}
	}
	parts = append(parts, cur.String())
	if len(parts) == 1 {
		return parts[0] == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, last)
}
