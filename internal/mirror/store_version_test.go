package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionedSpec is ingestSpec with a version column the delivery also sets.
func versionedSpec() *Spec {
	spec := ingestSpec()
	res := spec.Resources[0]
	res.Fields = append(res.Fields, Field{Name: "updated", Type: FieldTime, From: "updated_at", Version: true})
	ev := spec.Events.List[0]
	ev.Sets = append(ev.Sets, Set{Field: "updated", From: "repository.updated_at"})
	return spec
}

func TestVersion_ALateDeliveryDoesNotOverwriteANewerFetch(t *testing.T) {
	spec := versionedSpec()
	in, store := newIngest(t, spec)
	fetched := Row{"owner": "acme", "name": "widget", "visibility": "private", "stars": int64(9), "updated": clockLate}
	require.NoError(t, store.Put(context.Background(), spec.Resources[0], fetched, time.Now()))

	w := deliver(t, in, "repository", repoDelivery("acme", "widget", clockEarly,
		map[string]any{"visibility": "public", "stargazers_count": 1}))

	assert.Contains(t, w.Body.String(), string(DeliverySuperseded))
	row := repoRow(t, store, "acme", "widget")
	assert.Equal(t, "private", row["visibility"], "the fetched view is later and stays")
	assert.EqualValues(t, clockLate, row["updated"])
}

func TestVersion_PutOrdersByTheVersionColumn(t *testing.T) {
	ctx := context.Background()
	spec := versionedSpec()
	res := spec.Resources[0]
	store := openStore(t, dbPath(t), spec)
	put := func(visibility string, updated any) error {
		return store.Put(ctx, res, Row{"owner": "o", "name": "n", "visibility": visibility, "updated": updated}, time.Now())
	}

	require.NoError(t, put("public", clockLate))
	assert.ErrorIs(t, put("older", clockEarly), errStoredIsNewer)
	assert.Equal(t, "public", repoRow(t, store, "o", "n")["visibility"])

	require.NoError(t, put("same-moment", clockLate), "an equal version describes the same moment and applies")
	require.NoError(t, put("unversioned", nil), "a write with no version cannot be ordered and applies")
	assert.Equal(t, "unversioned", repoRow(t, store, "o", "n")["visibility"])
}

func TestVersion_RefusesTwoClocksAndATextClock(t *testing.T) {
	spec := versionedSpec()
	spec.Resources[0].Fields[0].Version = true
	assert.ErrorContains(t, spec.Resources[0].validate(), "more than one")

	spec = versionedSpec()
	spec.Resources[0].Fields[3].Type = FieldText
	assert.ErrorContains(t, spec.Resources[0].validate(), "time or an int")
}
