package mirror

import (
	"net/http"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestARestParameterBindsTheRemainingSegments(t *testing.T) {
	params, ok := matchPath("/repos/{owner}/{repo}/contents/{path...}", "/repos/acme/tools/contents/docs/a%20b/c.md")
	require.True(t, ok)
	assert.Equal(t, "docs/a%20b/c.md", params["path"])

	params, ok = matchPath("/repos/{owner}/{repo}/contents/{path...}", "/repos/acme/tools/contents/README.md")
	require.True(t, ok)
	assert.Equal(t, "README.md", params["path"])

	_, ok = matchPath("/repos/{owner}/{repo}/contents/{path...}", "/repos/acme/tools/contents")
	assert.False(t, ok, "a rest parameter binds at least one segment")

	_, ok = matchPath("/repos/{owner}/{repo}/contents/{path...}", "/repos/acme/tools/contents/a//b")
	assert.False(t, ok, "an empty segment binds nothing")
}

func TestFillPathKeepsARestValuesSlashes(t *testing.T) {
	rt := &Route{Path: "/repos/{owner}/{repo}/tarball/{ref...}"}
	path, ok := rt.fillPath(map[string]string{"owner": "acme", "repo": "tools", "ref": "cmd/v1.0 x"})
	require.True(t, ok)
	assert.Equal(t, "/repos/acme/tools/tarball/cmd/v1.0%20x", path)
}

func TestValidateRefusesARestParameterBeforeTheEnd(t *testing.T) {
	s := testSpec("http://example.invalid")
	s.Routes[0].Path = "/widgets/{id...}/more"
	assert.ErrorContains(t, s.validate(), "must be the last segment")
}

func TestValidateRefusesAMatchOnNoParameter(t *testing.T) {
	s := testSpec("http://example.invalid")
	s.Routes[0].Match = []ParamMatch{{Param: "nope", Pattern: regexp.MustCompile(".")}}
	assert.ErrorContains(t, s.validate(), "names no path parameter")
}

func TestAMatchSendsOtherShapesToTheNextRoute(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"id":"x","title":"t"}`))
	}), func(s *Spec) {
		short := *s.Resources[0]
		short.Name = "widget_short"
		short.TTL = time.Minute
		s.Resources = append(s.Resources, &short)
		s.Routes[0].Match = []ParamMatch{{Param: "id", Pattern: regexp.MustCompile(`^[0-9]+$`)}}
		s.Routes = append(s.Routes, &Route{Method: "GET", Path: "/widgets/{id}", Resource: "widget_short"})
	})

	assert.Equal(t, "widget", serveOne(t, e, "/widgets/42"), "a number has the shape the first route asks for")
	assert.Equal(t, "widget_short", serveOne(t, e, "/widgets/main"), "any other shape falls to the next route")
}

// serveOne drives one request and reports the resource that answered it.
func serveOne(t *testing.T, e *Engine, path string) string {
	t.Helper()
	rec := get(t, e, path)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	last := e.tel.Requests.Recent(1)
	require.Len(t, last, 1)
	return last[0].Resource
}
