package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandles_AnActionListTakesOnlyTheActionsItNames(t *testing.T) {
	ev := &Event{Type: "repository", Actions: []string{"deleted", "renamed"}}

	ok, err := handles(ev, map[string]any{"action": "renamed"})
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = handles(ev, map[string]any{"action": "edited"})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHandles_AWhenPredicateSplitsOneTypeByPayload(t *testing.T) {
	tip := &Event{Type: "push", When: `{{ and (hasPrefix "refs/heads/" .payload.ref) (not .payload.deleted) }}`}
	gone := &Event{Type: "push", When: `{{ and (hasPrefix "refs/heads/" .payload.ref) .payload.deleted }}`}

	cases := []struct {
		name    string
		payload map[string]any
		tip     bool
		gone    bool
	}{
		{"a branch push moves the tip", map[string]any{"ref": "refs/heads/main", "deleted": false}, true, false},
		{"a deleting push drops the row", map[string]any{"ref": "refs/heads/main", "deleted": true}, false, true},
		{"a tag push is neither", map[string]any{"ref": "refs/tags/v1", "deleted": false}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := handles(tip, c.payload)
			require.NoError(t, err)
			assert.Equal(t, c.tip, ok)
			ok, err = handles(gone, c.payload)
			require.NoError(t, err)
			assert.Equal(t, c.gone, ok)
		})
	}
}

func TestHandles_APredicateThatCannotRenderFails(t *testing.T) {
	ev := &Event{Type: "push", When: `{{ hasPrefix }}`}
	_, err := handles(ev, map[string]any{})
	assert.Error(t, err)
}
