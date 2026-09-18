package mirror

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAliasWriter_RepeatsTheHeaderUnderItsOldName(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &aliasWriter{ResponseWriter: rec, aliases: []HeaderAlias{{From: "X-Mirror-Cache", To: "X-GSM-Cache"}}}
	w.Header().Set("X-Mirror-Cache", "hit")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
	assert.Equal(t, "hit", rec.Header().Get("X-GSM-Cache"))
	assert.Equal(t, "hit", rec.Header().Get("X-Mirror-Cache"))

	absent := httptest.NewRecorder()
	w = &aliasWriter{ResponseWriter: absent, aliases: []HeaderAlias{{From: "X-Mirror-Stale", To: "X-GSM-Stale"}}}
	w.Write([]byte("{}"))
	_, present := absent.Header()["X-Gsm-Stale"]
	assert.False(t, present, "an alias of a header never set is not invented")
}
