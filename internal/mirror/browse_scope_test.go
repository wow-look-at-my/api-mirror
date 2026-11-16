package mirror

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestScopeOf_ReadsOnlyTheResourcesOwnColumnsAndFoldsKeys(t *testing.T) {
	res := &Resource{
		Name:   "pull",
		Keys:   []Key{{Name: "owner", Fold: true}, {Name: "repo", Fold: true}, {Name: "number"}},
		Fields: []Field{{Name: "state"}},
	}
	r := httptest.NewRequest("GET", "/_mirror/api/resources?resource=pull&owner=Acme&state=open&limit=5", nil)
	assert.Equal(t, map[string]string{"owner": "acme", "state": "open"}, scopeOf(r, res))
}

func TestInScope_MatchesEveryScopedComponent(t *testing.T) {
	res := &Resource{Name: "pull", Keys: []Key{{Name: "owner"}, {Name: "repo"}, {Name: "number"}}}
	key := keyString(res, map[string]string{"owner": "acme", "repo": "widget", "number": "7"})
	assert.True(t, inScope(res, key, map[string]string{"owner": "acme"}))
	assert.False(t, inScope(res, key, map[string]string{"owner": "beta"}))
	assert.False(t, inScope(nil, key, map[string]string{"owner": "acme"}), "a kind with no resource is outside any scope")
}

func TestTelemetry_LastSeenStaysBounded(t *testing.T) {
	tel := NewTelemetry(RateHeaders{})
	start := time.Now()
	for i := range seenMax + 1 {
		tel.Seen("token:"+itoa(i), start.Add(time.Duration(i)*time.Second))
	}
	seen := tel.LastSeen()
	assert.LessOrEqual(t, len(seen), seenMax/2+1)
	assert.Contains(t, seen, "token:"+itoa(seenMax), "the newest principal survives the trim")
	assert.NotContains(t, seen, "token:0", "the quietest principal goes first")
}
