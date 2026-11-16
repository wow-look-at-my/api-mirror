package delta

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonResp(body string) *Response {
	return &Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func diffsOf(t *testing.T, truth, mirror string) map[string]Diff {
	t.Helper()
	out := map[string]Diff{}
	for _, d := range compareResponses(jsonResp(truth), jsonResp(mirror), compareOpts{headers: []string{"Content-Type"}}) {
		out[string(d.Kind)+" "+d.Path] = d
	}
	return out
}

func TestCompareJSON(t *testing.T) {
	ds := diffsOf(t,
		`{"a":1,"b":"x","c":[1,2,3],"d":{"e":true},"f":1.0,"g":null}`,
		`{"a":2,"b":"x","c":[1,2],"d":{"e":true,"z":1},"f":1,"g":"s"}`)
	require.Len(t, ds, 4)
	assert.Equal(t, `1`, string(ds["changed $.a"].Truth))
	assert.Equal(t, `2`, string(ds["changed $.a"].Mirror))
	assert.Equal(t, `3`, string(ds["missing $.c[2]"].Truth))
	assert.Empty(t, ds["missing $.c[2]"].Mirror)
	assert.Equal(t, `1`, string(ds["extra $.d.z"].Mirror))
	assert.Equal(t, `"s"`, string(ds["type $.g"].Mirror))
}

func TestCompareEqualJSONIgnoresLayout(t *testing.T) {
	assert.Empty(t, diffsOf(t, `{"a": [1, {"b": 2}]}`, "{\"a\":[1,{\"b\":2}]}\n"))
}

func TestCompareStatusAndHeaders(t *testing.T) {
	truth := jsonResp(`{}`)
	truth.Header.Set("Link", `<https://api.example/x?page=2>; rel="next"`)
	mirror := jsonResp(`{}`)
	mirror.Status = 404
	mirror.Header.Set("Link", `<http://127.0.0.1:9/x?page=2>; rel="next"`)
	o := compareOpts{headers: []string{"link"}, truthBase: "https://api.example", mirrorBase: "http://127.0.0.1:9"}
	ds := compareResponses(truth, mirror, o)
	require.Len(t, ds, 1, "the Link headers agree once each base URL is normalized")
	assert.Equal(t, KindStatus, ds[0].Kind)

	mirror.Header.Set("Link", `<http://127.0.0.1:9/x?page=3>; rel="next"`)
	ds = compareResponses(truth, mirror, o)
	require.Len(t, ds, 2)
	assert.Equal(t, KindHeader, ds[1].Kind)
	assert.Equal(t, "Link", ds[1].Path)
	assert.Equal(t, `"<{base}/x?page=2>; rel=\"next\""`, string(ds[1].Truth))
}

func TestCompareNormalizesBodyStrings(t *testing.T) {
	o := compareOpts{truthBase: "https://api.example", mirrorBase: "http://m"}
	assert.Empty(t, compareResponses(jsonResp(`{"url":"https://api.example/r"}`), jsonResp(`{"url":"http://m/r"}`), o))
	ds := compareResponses(jsonResp(`{"url":"https://api.example/r"}`), jsonResp(`{"url":"https://api.example/r"}`), o)
	require.Len(t, ds, 1, "a mirror that echoes the truth's own URL sends clients past itself")
	assert.Equal(t, `"https://api.example/r"`, string(ds[0].Mirror))
}

func TestCompareNonJSON(t *testing.T) {
	a := &Response{Status: 200, Body: []byte("hello world")}
	b := &Response{Status: 200, Body: []byte("hello there")}
	ds := compareResponses(a, b, compareOpts{})
	require.Len(t, ds, 1)
	assert.Equal(t, KindBody, ds[0].Kind)
	assert.Equal(t, "byte 6", ds[0].Path)
	assert.Empty(t, compareResponses(a, &Response{Status: 200, Body: []byte("hello world")}, compareOpts{}))
}

func TestCompareHeadSkipsBody(t *testing.T) {
	assert.Empty(t, compareResponses(jsonResp(`{"a":1}`), jsonResp(`{"a":2}`), compareOpts{head: true}))
}

func TestCompareError(t *testing.T) {
	ds := compareResponses(jsonResp(`{}`), &Response{Err: errors.New("connection refused")}, compareOpts{})
	require.Len(t, ds, 1)
	assert.Equal(t, KindError, ds[0].Kind)
	assert.Empty(t, ds[0].Truth)
	assert.Equal(t, `"connection refused"`, string(ds[0].Mirror))
}

func TestLargeValuesAreMarkedTruncated(t *testing.T) {
	ds := diffsOf(t, string(mustJSON(map[string]string{"a": strings.Repeat("x", 3000)})), `{}`)
	d := ds["missing $.a"]
	assert.True(t, d.Truncated)
	assert.LessOrEqual(t, len(d.Truth), maxValue+16)
}

func TestShapeGroupsIndices(t *testing.T) {
	ds := diffsOf(t, `[{"u":1},{"u":2}]`, `[{"u":3},{"u":4}]`)
	require.Len(t, ds, 2)
	for _, d := range ds {
		assert.Equal(t, "$[*].u", d.Shape())
	}
}
