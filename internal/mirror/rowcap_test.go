package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRowCap_EvictsTheOldestRowsAndTheirFreshness(t *testing.T) {
	ctx := context.Background()
	spec := repoSpec()
	res := spec.Resources[0]
	s := openStore(t, dbPath(t), spec)
	s.maxRows = 2
	base := time.Now()

	for i, name := range []string{"old", "mid", "new"} {
		at := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, s.RecordFetched(ctx, Freshness{Kind: "repo", Key: "o/" + name, FetchedAt: at,
			ChangedAt: at, ExpiresAt: at.Add(time.Hour), State: "fresh"}))
		require.NoError(t, s.Put(ctx, res, Row{"owner": "o", "repo": name, "visibility": "public", "stars": int64(i)}, at))
	}

	rows, err := s.List(ctx, res, map[string]string{"owner": "o"})
	require.NoError(t, err)
	var kept []any
	for _, r := range rows {
		kept = append(kept, r["repo"])
	}
	assert.ElementsMatch(t, []any{"mid", "new"}, kept, "the least recently written row goes")

	gone, err := s.Freshness(ctx, "repo", "o/old")
	require.NoError(t, err)
	assert.Nil(t, gone, "an evicted row must read as a miss, never as fresh and empty")
	held, err := s.Freshness(ctx, "repo", "o/new")
	require.NoError(t, err)
	assert.NotNil(t, held)
}

func TestRowCap_RefusesACeilingBelowOne(t *testing.T) {
	err := run([]string{"api-mirror", "--max-rows", "0"}, nil)
	assert.ErrorContains(t, err, "--max-rows")
}
