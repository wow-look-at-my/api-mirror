package delta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loc(segs ...any) Location {
	var l Location
	for _, s := range segs {
		switch v := s.(type) {
		case string:
			l = append(l, keySeg(v))
		case int:
			l = append(l, indexSeg(v))
		}
	}
	return l
}

func TestLocationRendering(t *testing.T) {
	l := loc("items", 3, "a b", "it's", "x1")
	assert.Equal(t, `$.items[3]['a b']['it\'s'].x1`, l.String())
	assert.Equal(t, `$.items[*]['a b']['it\'s'].x1`, l.Shape())
	assert.Equal(t, "$", Location(nil).String())
}

func TestPatternCovers(t *testing.T) {
	cases := []struct {
		pattern string
		loc     Location
		want    bool
	}{
		{"$", loc("a"), true},
		{"$.a", loc("a"), true},
		{"$.a", loc("a", "b", 2), true},
		{"$.a", loc("b"), false},
		{"$.a.b", loc("a"), false},
		{"$.a[2]", loc("a", 2), true},
		{"$.a[2]", loc("a", 3), false},
		{"$.a[*].url", loc("a", 7, "url"), true},
		{"$.a.*.url", loc("a", 7, "url"), true},
		{"$.a.*.url", loc("a", "k", "url"), true},
		{"$..url", loc("x", 1, "y", "url"), true},
		{"$..url", loc("url"), true},
		{"$..url", loc("urls"), false},
		{"$..*_url", loc("owner", "html_url"), true},
		{"$..*_url", loc("owner", "url"), false},
		{"$..owner.id", loc(0, "owner", "id"), true},
		{"$..owner.id", loc(0, "owner", "login"), false},
		{"$['a b']", loc("a b", "c"), true},
		{"$['a*']", loc("abc"), false},
		{"$['a*']", loc("a*"), true},
		{"$.a..[0]", loc("a", "b", 0), true},
		{"$.a[0]", loc("a", "0"), false},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.pattern)
		require.NoError(t, err, c.pattern)
		assert.Equal(t, c.want, p.Covers(c.loc), "%s covers %s", c.pattern, c.loc)
	}
}

func TestParsePatternRejects(t *testing.T) {
	for _, src := range []string{"a.b", "$.", "$.a[", "$.a[?(@.x)]", "$.a[1:2]", "$['a", "$.a[-1]", "$x"} {
		_, err := ParsePattern(src)
		assert.Error(t, err, src)
	}
}

func TestGlobMatch(t *testing.T) {
	assert.True(t, globMatch("*", ""))
	assert.True(t, globMatch("a*b*c", "aXXbYYc"))
	assert.False(t, globMatch("a*b*c", "aXXcYYb"))
	assert.False(t, globMatch("ab*b", "ab"))
	assert.True(t, globMatch("*_url", "_url"))
}
