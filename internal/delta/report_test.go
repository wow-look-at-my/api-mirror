package delta

import (
	"bytes"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/go-containers/set"
)

func TestRecentRingKeepsTheNewestAndCountsEvictions(t *testing.T) {
	rep, err := NewReporter(slog.New(slog.NewTextHandler(io.Discard, nil)), "", 3, nil)
	require.NoError(t, err)
	for i := range 5 {
		rep.Add(Record{Path: "/" + strconv.Itoa(i)})
	}
	recs := rep.Recent(10)
	require.Len(t, recs, 3)
	assert.Equal(t, []string{"/4", "/3", "/2"}, []string{recs[0].Path, recs[1].Path, recs[2].Path})
	assert.Equal(t, 2, rep.Summary().Evicted)
}

func TestSummaryCapLogsWhatItDrops(t *testing.T) {
	var logs bytes.Buffer
	rep, err := NewReporter(slog.New(slog.NewTextHandler(&logs, nil)), "", 0, nil)
	require.NoError(t, err)
	for i := range maxRows + 2 {
		rep.Add(Record{Method: "GET", Path: "/" + strconv.Itoa(i), Diffs: []Diff{{Kind: KindStatus}}})
	}
	s := rep.Summary()
	assert.Len(t, s.Rows, maxRows)
	assert.Equal(t, 2, s.DroppedRows)
	assert.Contains(t, logs.String(), "summary is full")
	var text bytes.Buffer
	require.NoError(t, s.WriteText(&text))
	assert.Contains(t, text.String(), "WARNING: 2 difference classes")
}

func TestSummaryNamesARuleThatMatchedNothing(t *testing.T) {
	rules := []*Rule{{Path: "$.a", Reason: "why"}}
	require.NoError(t, rules[0].validate(set.Of(TruthName)))
	rep, err := NewReporter(slog.New(slog.NewTextHandler(io.Discard, nil)), "", 1, rules)
	require.NoError(t, err)
	var text bytes.Buffer
	require.NoError(t, rep.Summary().WriteText(&text))
	assert.Contains(t, text.String(), "(matched nothing)  # why")
}
