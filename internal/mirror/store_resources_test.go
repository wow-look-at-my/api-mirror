package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-resource half of the store: tables no generator can type, driven by
// statements built from columnsOf. The shared helpers live in store_test.go.

func TestResourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	r, ok := s.Resource("repo")
	require.True(t, ok)

	putRepo(t, s, r, "wow", "api-mirror", "public", 7)
	putRepo(t, s, r, "wow", "api-cli", "private", 3)
	putRepo(t, s, r, "other", "thing", "public", 1)

	row, err := s.Get(ctx, r, map[string]string{"owner": "wow", "repo": "api-cli"})
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "private", row["visibility"])
	assert.Equal(t, int64(3), row["stars"])

	missing, err := s.Get(ctx, r, map[string]string{"owner": "wow", "repo": "nope"})
	require.NoError(t, err)
	assert.Nil(t, missing, "an absent row is an answer, not an error")

	// A partial key lists owner, in key order, and nobody else's rows.
	list, err := s.List(ctx, r, map[string]string{"owner": "wow"})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "api-cli", list[0]["repo"])
	assert.Equal(t, "api-mirror", list[1]["repo"])

	all, err := s.List(ctx, r, nil)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	n, err := s.Delete(ctx, r, map[string]string{"owner": "wow"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	left, err := s.List(ctx, r, nil)
	require.NoError(t, err)
	assert.Len(t, left, 1)
}

// An empty key must not read as "delete the table".
func TestDeleteRefusesEmptyKey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	r, ok := s.Resource("repo")
	require.True(t, ok)
	putRepo(t, s, r, "wow", "api-mirror", "public", 7)

	_, err := s.Delete(ctx, r, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every row")

	left, err := s.List(ctx, r, nil)
	require.NoError(t, err)
	assert.Len(t, left, 1, "the refusal must leave the rows alone")
}

func TestPutManyIsOneList(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	r, ok := s.Resource("repo")
	require.True(t, ok)

	rows := []Row{
		{"owner": "wow", "repo": "a", "visibility": "public", "stars": int64(1)},
		{"owner": "wow", "repo": "b", "visibility": "public", "stars": int64(2)},
	}
	require.NoError(t, s.PutMany(ctx, r, rows, time.Unix(1700000000, 0)))

	got, err := s.List(ctx, r, map[string]string{"owner": "wow"})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestGetRequiresAKey(t *testing.T) {
	s := openStore(t, dbPath(t), repoSpec())
	r, ok := s.Resource("repo")
	require.True(t, ok)

	_, err := s.Get(context.Background(), r, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no key supplied")
}

// A document resource stores the body itself, so its table has payload
// column instead of the declared fields.
func TestDocumentResourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	spec := &Spec{
		Name: "test",
		Resources: []*Resource{{
			Name:  "blob",
			Store: StoreDocument,
			Keys:  []Key{{Name: "id"}},
		}},
	}
	s := openStore(t, dbPath(t), spec)
	r, ok := s.Resource("blob")
	require.True(t, ok)

	require.NoError(t, s.Put(ctx, r, Row{"id": "1", "document": `{"a":1}`}, time.Unix(1700000000, 0)))
	row, err := s.Get(ctx, r, map[string]string{"id": "1"})
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, `{"a":1}`, row["document"])
}
