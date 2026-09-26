package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
