package mirror

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmin_CompressesBufferedJSONForABrowserThatAsks(t *testing.T) {
	a := &Admin{engine: &Engine{spec: &Spec{}}, prefix: "/_mirror", token: "tok", mux: http.NewServeMux()}
	a.routes()

	r := httptest.NewRequest(http.MethodGet, "/_mirror/api/whoami", nil)
	r.Header.Set("X-Mirror-Token", "tok")
	r.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))

	zr, err := gzip.NewReader(w.Body)
	require.NoError(t, err)
	body, err := io.ReadAll(zr)
	require.NoError(t, err)
	var who WhoAmI
	require.NoError(t, json.Unmarshal(body, &who))
	assert.True(t, who.Admin)

	r = httptest.NewRequest(http.MethodGet, "/_mirror/api/whoami", nil)
	r.Header.Set("X-Mirror-Token", "tok")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	assert.Empty(t, w.Header().Get("Content-Encoding"), "a caller that did not ask gets plain JSON")
}

func TestCompressible_LeavesStreamsAndPagesAlone(t *testing.T) {
	ask := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		return r
	}
	assert.True(t, compressible(ask("/_mirror/api/overview"), "/_mirror"))
	assert.False(t, compressible(ask("/_mirror/api/check?kind=repo&stream=1"), "/_mirror"), "a stream must keep flushing")
	assert.False(t, compressible(ask("/_mirror/app.js"), "/_mirror"))
	assert.False(t, compressible(ask("/repos/a/b"), "/_mirror"), "the data plane is never wrapped")
}
