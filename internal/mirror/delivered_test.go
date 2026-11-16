package mirror

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliveryStatingEveryFieldIsFreshWithoutAFetch(t *testing.T) {
	in, store := newIngest(t, ingestSpec())
	require.Equal(t, http.StatusOK, deliver(t, in, "repository",
		repoDelivery("acme", "widget", clockLate, map[string]any{"visibility": "public", "stargazers_count": 3, "topic": "x"})).Code)

	res, ok := store.Resource("repo")
	require.True(t, ok)
	meta, err := store.Freshness(context.Background(), "repo", keyString(res, map[string]string{"owner": "acme", "name": "widget"}))
	require.NoError(t, err)
	require.NotNil(t, meta, "a push states the whole tip; a read after it must not ask the upstream again")
	assert.Equal(t, string(StateFresh), meta.State)
	assert.True(t, meta.ExpiresAt.After(meta.FetchedAt))
}

func TestPartialDeliveryLeavesFreshnessAlone(t *testing.T) {
	spec := ingestSpec()
	spec.Events.List[0].Sets = spec.Events.List[0].Sets[:1]
	in, store := newIngest(t, spec)
	require.Equal(t, http.StatusOK, deliver(t, in, "repository",
		repoDelivery("acme", "widget", clockLate, map[string]any{"visibility": "public"})).Code)

	res, ok := store.Resource("repo")
	require.True(t, ok)
	meta, err := store.Freshness(context.Background(), "repo", keyString(res, map[string]string{"owner": "acme", "name": "widget"}))
	require.NoError(t, err)
	assert.Nil(t, meta, "a row holding one field of three must still fetch the other two")
}
