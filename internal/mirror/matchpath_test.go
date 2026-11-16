package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMatchPath_ARestParamTakesEveryRemainingSegment(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          map[string]string
	}{
		{"/repos/{owner}/{repo}/contents/{path*}", "/repos/o/r/contents/docs/a/b.md",
			map[string]string{"owner": "o", "repo": "r", "path": "docs/a/b.md"}},
		{"/repos/{owner}/{repo}/contents/{path*}", "/repos/o/r/contents/README.md",
			map[string]string{"owner": "o", "repo": "r", "path": "README.md"}},
		{"/repos/{owner}/{repo}/git/ref/{ref*}", "/repos/o/r/git/ref/heads/feature/x",
			map[string]string{"owner": "o", "repo": "r", "ref": "heads/feature/x"}},
		{"/repos/{owner}/{repo}/compare/{basehead*}", "/repos/o/r/compare/main...user/topic",
			map[string]string{"owner": "o", "repo": "r", "basehead": "main...user/topic"}},
		{"/repos/{owner}/{repo}", "/repos/o/r", map[string]string{"owner": "o", "repo": "r"}},
	}
	for _, tc := range cases {
		got, ok := matchPath(tc.pattern, tc.path)
		assert.True(t, ok, tc.path)
		assert.Equal(t, tc.want, got, tc.path)
	}
}

func TestMatchPath_ARestParamRefusesWhatIsNotAValue(t *testing.T) {
	for _, path := range []string{
		"/repos/o/r/contents",
		"/repos/o/r/contents/",
		"/repos/o/r/contents/a//b",
	} {
		_, ok := matchPath("/repos/{owner}/{repo}/contents/{path*}", path)
		assert.False(t, ok, "%s names no file", path)
	}
	_, ok := matchPath("/repos/{owner}/{repo}", "/repos/o/r/extra")
	assert.False(t, ok, "a plain param still binds exactly one segment")
}
