package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func displayResource() *Resource {
	return &Resource{
		Name:   "repo",
		Store:  StoreColumns,
		Keys:   []Key{{Name: "owner", From: "owner.login", Fold: true}, {Name: "name", From: "name"}},
		Fields: []Field{{Name: "description", Type: FieldText, From: "description"}},
	}
}

// ownerSent is the engine column keeping a folded key's as-sent spelling.
func ownerSent() string {
	return Key{Name: "owner", From: "owner.login", Fold: true}.sentColumn()
}

func TestRebuild_AFoldedKeyAnswersTheSpellingTheUpstreamStated(t *testing.T) {
	row := Row{"owner": "acme", "name": "widget", "description": "a widget"}
	row[ownerSent()] = "Acme"
	doc, err := rebuild(displayResource(), row)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"owner":       map[string]any{"login": "Acme"},
		"name":        "widget",
		"description": "a widget",
	}, doc, "the folded key answers the spelling the upstream stated, not the one it is stored under")
}

func TestRebuild_ANullSentColumnLeavesTheKeyOut(t *testing.T) {
	doc, err := rebuild(displayResource(), Row{"owner": "acme", "name": "widget", "description": "a widget"})
	require.NoError(t, err)
	assert.NotContains(t, doc, "owner", "the folded value is not the upstream's word, so it is not served as one")
	assert.Equal(t, "widget", doc["name"])
}

func TestRebuild_ANullFieldIsNotServed(t *testing.T) {
	doc, err := rebuild(displayResource(), Row{"owner": "acme", "name": "widget", "description": nil})
	require.NoError(t, err)
	assert.NotContains(t, doc, "description", "a null cell may be a value the upstream never sent")
	assert.Contains(t, doc, "name")
}
