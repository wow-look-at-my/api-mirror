package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func displayResource() *Resource {
	return &Resource{
		Name:  "repo",
		Store: StoreColumns,
		Keys:  []Key{{Name: "owner", From: "owner.login", Fold: true}, {Name: "name", From: "name", Fold: true}},
		Fields: []Field{
			{Name: "owner_login", Type: FieldText, From: "owner.login"},
			{Name: "name_given", Type: FieldText, From: "name"},
		},
	}
}

func TestRebuild_ADisplayColumnAnswersTheSpellingTheUpstreamStated(t *testing.T) {
	doc, err := rebuild(displayResource(), Row{"owner": "acme", "name": "widget", "owner_login": "Acme", "name_given": "Widget"})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"owner": map[string]any{"login": "Acme"}, "name": "Widget"}, doc)
}

func TestRebuild_ANullDisplayColumnLeavesTheKey(t *testing.T) {
	doc, err := rebuild(displayResource(), Row{"owner": "acme", "name": "widget", "owner_login": nil, "name_given": nil})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"owner": map[string]any{"login": "acme"}, "name": "widget"}, doc)
}

func TestRebuild_ANullFieldAloneIsStillAnsweredNull(t *testing.T) {
	r := displayResource()
	r.Fields = append(r.Fields, Field{Name: "description", Type: FieldText, From: "description"})
	doc, err := rebuild(r, Row{"owner": "acme", "name": "widget", "description": nil})
	require.NoError(t, err)
	assert.Contains(t, doc, "description")
	assert.Nil(t, doc["description"])
}
