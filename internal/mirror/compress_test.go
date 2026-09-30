package mirror

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminJSONIsGzippedForACallerThatAcceptsIt(t *testing.T) {
	e, _ := newTestEngine(t, http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/_mirror/api/overview", nil)
	req.Header.Set("X-Mirror-Token", e.admin.token)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	gz, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	body, err := io.ReadAll(gz)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"mirror"`)

	plain := httptest.NewRequest(http.MethodGet, "/_mirror/api/overview", nil)
	plain.Header.Set("X-Mirror-Token", e.admin.token)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, plain)
	assert.Empty(t, rec.Header().Get("Content-Encoding"), "a caller that did not ask gets plain JSON")
	assert.Contains(t, rec.Body.String(), `"mirror"`)
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{"gzip": true, "br, gzip;q=0.5": true, "gzip;q=0": false, "": false, "deflate": false} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", header)
		assert.Equalf(t, want, acceptsGzip(r), "%q", header)
	}
}
