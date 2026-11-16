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

func TestRowCapDropsTheOldestWrites(t *testing.T) {
	spec := testSpec("https://api.example.com")
	spec.MaxRows = 2
	store := openStore(t, dbPath(t), spec)
	res, ok := store.Resource("widget")
	require.True(t, ok)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	for i, id := range []string{"old", "mid", "new"} {
		// Each write checks the cap only once per interval, so reset the stamp.
		store.cap.checked = map[string]time.Time{}
		require.NoError(t, store.Put(ctx, res, Row{"id": id, "title": id}, base.Add(time.Duration(i)*time.Minute)))
	}

	for id, want := range map[string]bool{"old": false, "mid": true, "new": true} {
		row, err := store.Get(ctx, res, map[string]string{"id": id})
		require.NoError(t, err)
		assert.Equalf(t, want, row != nil, "row %s", id)
	}
}

func TestMaxRowsParses(t *testing.T) {
	n, err := parseMaxRows("")
	require.NoError(t, err)
	assert.Equal(t, defaultMaxRows, n, "the cap is on by default")

	t.Setenv("MIRROR_TEST_MAX_ROWS", "500")
	n, err = parseMaxRows(`{{ .env.MIRROR_TEST_MAX_ROWS }}`)
	require.NoError(t, err)
	assert.Equal(t, 500, n)

	for _, bad := range []string{"0", "-1", "lots"} {
		_, err := parseMaxRows(bad)
		assert.Errorf(t, err, "%q must fail the load", bad)
	}
}
