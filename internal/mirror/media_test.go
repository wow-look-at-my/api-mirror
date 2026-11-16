package mirror

import _ "embed"

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getAccept drives a GET with an Accept header through the engine.
func getAccept(t *testing.T, e *Engine, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestMirrorAcceptNamesTheDefaultRepresentation(t *testing.T) {
	var calls atomic.Int32
	e, _ := newTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"id":"7","title":"hello"}`))
	}), func(s *Spec) {
		s.Routes[0].Accept = []string{"application/json"}
		s.Accept = []string{"application/vnd.example.preview+json", "application/vnd.example.other"}
	})

	rec := getAccept(t, e, "/widgets/7", "application/vnd.example.preview+json, application/vnd.example.other")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(OutcomeMiss), rec.Header().Get("X-Mirror-Cache"),
		"a media type the spec names as the default must be served from the cache")

	rec = getAccept(t, e, "/widgets/7", "application/vnd.example.diff")
	assert.Equal(t, string(PassAccept), rec.Header().Get("X-Mirror-Passthrough-Reason"),
		"a media type nobody declared is still forwarded")
}

func TestValidateRejectsAMirrorAcceptThatIsNotAMediaType(t *testing.T) {
	s := testSpec("http://example.invalid")
	s.Accept = []string{"json"}
	assert.ErrorContains(t, s.validate(), "is not a media type")
}

func TestLoadReadsTheMirrorAccept(t *testing.T) {
	spec, err := ParseSpec([]byte(testLoadReadsTheMirrorAcceptXml))
	require.NoError(t, err)
	assert.Equal(t, []string{"application/vnd.example+json"}, spec.Accept)
}

//go:embed testdata/test-load-reads-the-mirror-accept.xml
var testLoadReadsTheMirrorAcceptXml string
