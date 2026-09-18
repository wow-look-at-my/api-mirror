package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func contradictionSpec() *Spec {
	gate := &Reveal{Public: "true"}
	branch := &Resource{
		Name:  "branch",
		Store: StoreColumns,
		Keys:  []Key{{Name: "owner"}, {Name: "repo"}, {Name: "ref"}},
		Fields: []Field{
			{Name: "tip_sha", Type: FieldText, From: "object.sha"},
		},
		Reveal: gate,
		Contradiction: &Contradiction{
			Resource: "pull",
			Field:    "base_sha",
			Against:  "tip_sha",
			On:       []ContradictionJoin{{Key: "owner", Column: "owner"}, {Key: "repo", Column: "repo"}, {Key: "ref", Column: "base_ref"}},
			Only:     []ContradictionOnly{{Column: "state", Value: "open"}},
		},
	}
	pull := &Resource{
		Name:  "pull",
		Store: StoreColumns,
		Keys:  []Key{{Name: "owner"}, {Name: "repo"}, {Name: "number", Type: FieldInt}},
		Fields: []Field{
			{Name: "state", Type: FieldText, From: "state"},
			{Name: "base_ref", Type: FieldText, From: "base.ref"},
			{Name: "base_sha", Type: FieldText, From: "base.sha"},
		},
		Reveal: gate,
	}
	return &Spec{
		Name:      "t",
		Upstream:  Upstream{Base: "https://example.invalid"},
		Resources: []*Resource{branch, pull},
	}
}

func TestContradiction_AJoinMustCoverEveryKey(t *testing.T) {
	s := contradictionSpec()
	s.Resources[0].Contradiction.On = s.Resources[0].Contradiction.On[:2]
	assert.ErrorContains(t, s.validate(), `joins no column to key "ref"`)
}

func TestContradiction_NamesMustBeColumns(t *testing.T) {
	s := contradictionSpec()
	s.Resources[0].Contradiction.Field = "nope"
	assert.ErrorContains(t, s.validate(), "is not a column")
}

func TestContradiction_ALaterOpenPullNamingAnotherBaseRefetchesOnce(t *testing.T) {
	spec := contradictionSpec()
	require.NoError(t, spec.validate())
	store := openStore(t, dbPath(t), spec)
	ctx := context.Background()
	branch, _ := store.Resource("branch")
	pull, _ := store.Resource("pull")
	key := map[string]string{"owner": "acme", "repo": "widget", "ref": "main"}
	t0 := time.Now().Add(-time.Hour)
	require.NoError(t, store.Put(ctx, branch, Row{"owner": "acme", "repo": "widget", "ref": "main", "tip_sha": "old"}, t0))

	var settled settledValues
	v, err := store.contradicted(ctx, branch, key, &settled)
	require.NoError(t, err)
	assert.Empty(t, v, "no pull row says anything")

	require.NoError(t, store.Put(ctx, pull, Row{"owner": "acme", "repo": "widget", "number": "7", "state": "closed", "base_ref": "main", "base_sha": "new"}, t0.Add(time.Minute)))
	v, err = store.contradicted(ctx, branch, key, &settled)
	require.NoError(t, err)
	assert.Empty(t, v, "a closed pull's base is history")

	require.NoError(t, store.Put(ctx, pull, Row{"owner": "acme", "repo": "widget", "number": "8", "state": "open", "base_ref": "main", "base_sha": "new"}, t0.Add(time.Minute)))
	v, err = store.contradicted(ctx, branch, key, &settled)
	require.NoError(t, err)
	assert.Equal(t, "new", v)

	settled.settle("branch\x00"+keyString(branch, key), "new")
	v, err = store.contradicted(ctx, branch, key, &settled)
	require.NoError(t, err)
	assert.Empty(t, v, "a value a refetch already answered is not new evidence")
}

func TestContradiction_APullWrittenBeforeTheRowSaysNothing(t *testing.T) {
	spec := contradictionSpec()
	store := openStore(t, dbPath(t), spec)
	ctx := context.Background()
	branch, _ := store.Resource("branch")
	pull, _ := store.Resource("pull")
	t0 := time.Now().Add(-time.Hour)
	require.NoError(t, store.Put(ctx, pull, Row{"owner": "acme", "repo": "widget", "number": "8", "state": "open", "base_ref": "main", "base_sha": "lagging"}, t0))
	require.NoError(t, store.Put(ctx, branch, Row{"owner": "acme", "repo": "widget", "ref": "main", "tip_sha": "current"}, t0.Add(time.Minute)))

	v, err := store.contradicted(ctx, branch, map[string]string{"owner": "acme", "repo": "widget", "ref": "main"}, &settledValues{})
	require.NoError(t, err)
	assert.Empty(t, v)
}
