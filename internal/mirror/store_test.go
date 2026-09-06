package mirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoSpec is the spec every test opens against: keys, columns.
func repoSpec() *Spec {
	return &Spec{
		Name: "test",
		Resources: []*Resource{{
			Name:  "repo",
			Store: StoreColumns,
			TTL:   time.Hour,
			Keys:  []Key{{Name: "owner"}, {Name: "repo"}},
			Fields: []Field{
				{Name: "visibility", Type: FieldText, From: "visibility"},
				{Name: "stars", Type: FieldInt, From: "stargazers_count"},
			},
		}},
	}
}

// widerSpec is repoSpec with more column, which is a different schema and
// so a different fingerprint.
func widerSpec() *Spec {
	s := repoSpec()
	s.Resources[0].Fields = append(s.Resources[0].Fields,
		Field{Name: "default_branch", Type: FieldText, From: "default_branch"})
	return s
}

func openStore(t *testing.T, path string, spec *Spec) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, spec)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// dbPath is a fresh database file in this test's own directory.
func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "mirror.db")
}

// tableExists asks SQLite itself, so a test cannot pass on a table the engine
// only intended to create.
func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var got string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&got)
	if err != nil {
		return false
	}
	return got == name
}

func putRepo(t *testing.T, s *Store, r *Resource, owner, repo, vis string, stars int64) {
	t.Helper()
	row := Row{"owner": owner, "repo": repo, "visibility": vis, "stars": stars}
	require.NoError(t, s.Put(context.Background(), r, row, time.Unix(1700000000, 0)))
}

func TestOpenCreatesSchema(t *testing.T) {
	s := openStore(t, dbPath(t), repoSpec())

	for _, name := range []string{
		"mirror_meta", "mirror_freshness", "mirror_watermark", "mirror_grant", "mirror_deny",
	} {
		assert.True(t, tableExists(t, s, name), "engine table %s", name)
	}
	assert.True(t, tableExists(t, s, "res_repo"), "derived resource table")

	fp, err := s.Meta(context.Background(), "fingerprint")
	require.NoError(t, err)
	assert.Equal(t, repoSpec().Fingerprint(), fp)
}

func TestReopenSameSpecKeepsRows(t *testing.T) {
	path := dbPath(t)
	ctx := context.Background()

	first := openStore(t, path, repoSpec())
	r, ok := first.Resource("repo")
	require.True(t, ok)
	putRepo(t, first, r, "wow", "api-mirror", "public", 7)
	require.NoError(t, first.Close())

	second := openStore(t, path, repoSpec())
	r, ok = second.Resource("repo")
	require.True(t, ok)
	row, err := second.Get(ctx, r, map[string]string{"owner": "wow", "repo": "api-mirror"})
	require.NoError(t, err)
	require.NotNil(t, row, "a spec that did not change must not nuke")
	assert.Equal(t, "public", row["visibility"])
}

func TestReopenChangedSpecNukes(t *testing.T) {
	path := dbPath(t)
	ctx := context.Background()

	first := openStore(t, path, repoSpec())
	r, ok := first.Resource("repo")
	require.True(t, ok)
	putRepo(t, first, r, "wow", "api-mirror", "public", 7)
	require.NoError(t, first.Close())

	second := openStore(t, path, widerSpec())
	r, ok = second.Resource("repo")
	require.True(t, ok)

	row, err := second.Get(ctx, r, map[string]string{"owner": "wow", "repo": "api-mirror"})
	require.NoError(t, err)
	assert.Nil(t, row, "a changed spec must rebuild the cache empty")

	// The new column is the point: the old table could not hold it.
	putRepo(t, second, r, "wow", "api-cli", "public", 3)
	fresh, err := second.Get(ctx, r, map[string]string{"owner": "wow", "repo": "api-cli"})
	require.NoError(t, err)
	require.NotNil(t, fresh)
	assert.Contains(t, fresh, "default_branch")

	fp, err := second.Meta(ctx, "fingerprint")
	require.NoError(t, err)
	assert.Equal(t, widerSpec().Fingerprint(), fp)
}

// A sidecar left behind resurrects part of the old database, so the nuke has to
// take all files.
func TestNukeRemovesWALSidecars(t *testing.T) {
	path := dbPath(t)

	first := openStore(t, path, repoSpec())
	r, ok := first.Resource("repo")
	require.True(t, ok)
	putRepo(t, first, r, "wow", "api-mirror", "public", 7)
	// A live connection is what leaves a -wal and a -shm on disk.
	require.FileExists(t, path+"-wal")
	require.NoError(t, first.Close())

	// Re-create the sidecars so the nuke has something to remove.
	require.NoError(t, os.WriteFile(path+"-wal", []byte("stale"), 0o600))
	require.NoError(t, os.WriteFile(path+"-shm", []byte("stale"), 0o600))

	second := openStore(t, path, widerSpec())
	require.NoError(t, second.Close())

	for _, p := range []string{path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(p)
		if err == nil {
			assert.NotEqual(t, "stale", string(data), "%s survived the nuke", p)
		}
	}
}

func TestWatermarkOrdering(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())

	_, found, err := s.Watermark(ctx, "repo:wow/api-mirror")
	require.NoError(t, err)
	assert.False(t, found, "an untouched subject has no watermark")

	at := time.Unix(1700000000, 0)
	applied, err := s.ApplyWatermark(ctx, "repo:wow/api-mirror", at)
	require.NoError(t, err)
	assert.True(t, applied, "the first view always applies")

	// Equal MUST apply: the clock is a, and distinct views land
	applied, err = s.ApplyWatermark(ctx, "repo:wow/api-mirror", at)
	require.NoError(t, err)
	assert.True(t, applied, "an equal-time view must apply")

	applied, err = s.ApplyWatermark(ctx, "repo:wow/api-mirror", at.Add(-time.Minute))
	require.NoError(t, err)
	assert.False(t, applied, "an older view restates superseded state")

	stored, found, err := s.Watermark(ctx, "repo:wow/api-mirror")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, at.Unix(), stored.Unix(), "a refused view must not move the mark")

	applied, err = s.ApplyWatermark(ctx, "repo:wow/api-mirror", at.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, applied)

	n, err := s.PruneWatermarks(ctx, at.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	_, found, err = s.Watermark(ctx, "repo:wow/api-mirror")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestGrantExpires(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)

	require.NoError(t, s.RecordGrant(ctx, Grant{
		Principal: "user:1", Resource: "repo", Key: "wow/api-mirror",
		Source: "probe", ExpiresAt: now.Add(time.Hour),
	}))

	live, err := s.HasGrant(ctx, "user:1", "repo", "wow/api-mirror", now)
	require.NoError(t, err)
	assert.True(t, live)

	// The read filters on expiry, so the row is unusable before any prune runs.
	live, err = s.HasGrant(ctx, "user:1", "repo", "wow/api-mirror", now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.False(t, live, "an expired grant must never be served")

	other, err := s.HasGrant(ctx, "user:2", "repo", "wow/api-mirror", now)
	require.NoError(t, err)
	assert.False(t, other, "a grant proves one principal's access, not everyone's")

	n, err := s.PruneGrants(ctx, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestRecordGrantClearsDenial(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)

	require.NoError(t, s.RecordDenial(ctx, "user:1", "repo", "wow/secret", 404, now.Add(5*time.Minute)))
	status, found, err := s.Denial(ctx, "user:1", "repo", "wow/secret", now)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 404, status)

	// Upstream just said yes. A cached no beside it answers question.
	require.NoError(t, s.RecordGrant(ctx, Grant{
		Principal: "user:1", Resource: "repo", Key: "wow/secret",
		Source: "probe", ExpiresAt: now.Add(time.Hour),
	}))

	_, found, err = s.Denial(ctx, "user:1", "repo", "wow/secret", now)
	require.NoError(t, err)
	assert.False(t, found, "recording a grant must clear the matching denial")

	live, err := s.HasGrant(ctx, "user:1", "repo", "wow/secret", now)
	require.NoError(t, err)
	assert.True(t, live)
}

func TestDenialExpires(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)

	require.NoError(t, s.RecordDenial(ctx, "user:1", "repo", "wow/secret", 404, now.Add(5*time.Minute)))

	_, found, err := s.Denial(ctx, "user:1", "repo", "wow/secret", now.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, found, "an expired denial must be re-asked, not replayed")

	n, err := s.PruneDenials(ctx, now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

// A list answer states the whole set, so what it stops naming is gone.
func TestReplaceGrantsIsASync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)
	expires := now.Add(time.Hour)

	require.NoError(t, s.ReplaceGrants(ctx, "user:1", "repo", "list_sync",
		[]string{"wow/a", "wow/b"}, expires))
	// A probe grant is a different source and must survive the sync.
	require.NoError(t, s.RecordGrant(ctx, Grant{
		Principal: "user:1", Resource: "repo", Key: "wow/c",
		Source: "probe", ExpiresAt: expires,
	}))

	require.NoError(t, s.ReplaceGrants(ctx, "user:1", "repo", "list_sync",
		[]string{"wow/a"}, expires))

	kept, err := s.HasGrant(ctx, "user:1", "repo", "wow/a", now)
	require.NoError(t, err)
	assert.True(t, kept)

	dropped, err := s.HasGrant(ctx, "user:1", "repo", "wow/b", now)
	require.NoError(t, err)
	assert.False(t, dropped, "a key the sync stopped naming is access the caller lost")

	probe, err := s.HasGrant(ctx, "user:1", "repo", "wow/c", now)
	require.NoError(t, err)
	assert.True(t, probe, "another source's grant is not this sync's to delete")
}

func TestRevokeGrant(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)

	require.NoError(t, s.RecordGrant(ctx, Grant{
		Principal: "user:1", Resource: "repo", Key: "wow/a",
		Source: "probe", ExpiresAt: now.Add(time.Hour),
	}))

	gone, err := s.RevokeGrant(ctx, "user:1", "repo", "wow/a")
	require.NoError(t, err)
	assert.True(t, gone)

	gone, err = s.RevokeGrant(ctx, "user:1", "repo", "wow/a")
	require.NoError(t, err)
	assert.False(t, gone, "revoking nothing reports nothing")
}

func TestFreshnessLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	now := time.Unix(1700000000, 0)

	got, err := s.Freshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	assert.Nil(t, got, "a key never fetched has no marker")

	require.NoError(t, s.MarkFetching(ctx, "repo", "wow/api-mirror"))
	got, err = s.Freshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "fetching", got.State)

	require.NoError(t, s.MarkError(ctx, "repo", "wow/api-mirror", "502 upstream", now.Add(time.Minute)))
	got, err = s.Freshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "error", got.State)
	assert.Equal(t, "502 upstream", got.Error)
	assert.Equal(t, now.Add(time.Minute).Unix(), got.RetryAfter.Unix())

	// A fetch that answered clears the backoff. Holding the next off over a
	// failure that healed is the same outage.
	require.NoError(t, s.RecordFetched(ctx, Freshness{
		Kind: "repo", Key: "wow/api-mirror",
		FetchedAt: now, ChangedAt: now, ETag: `W/"abc"`,
		ExpiresAt: now.Add(time.Hour), State: "fresh",
	}))
	got, err = s.Freshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "fresh", got.State)
	assert.Empty(t, got.Error)
	assert.True(t, got.RetryAfter.IsZero(), "a healed fetch clears the backoff")
	assert.Equal(t, `W/"abc"`, got.ETag)
	assert.Equal(t, now.Unix(), got.FetchedAt.Unix())

	n, err := s.DeleteFreshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	got, err = s.Freshness(ctx, "repo", "wow/api-mirror")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// A name collision surfaces otherwise as a confusing SQL error at boot, so
// Open refuses the spec before it derives any DDL.
func TestOpenRejectsUnusableNames(t *testing.T) {
	cases := map[string]func(*Spec){
		"engine prefix": func(s *Spec) { s.Resources[0].Name = "mirror_repo" },
		"engine column": func(s *Spec) { s.Resources[0].Fields[0].Name = "mirror_written_at" },
		"sql keyword":   func(s *Spec) { s.Resources[0].Fields[0].Name = "order" },
		"leading digit": func(s *Spec) { s.Resources[0].Fields[0].Name = "1st" },
		"punctuation":   func(s *Spec) { s.Resources[0].Fields[0].Name = "we-ird" },
		"empty":         func(s *Spec) { s.Resources[0].Fields[0].Name = "" },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			spec := repoSpec()
			break_(spec)
			s, err := Open(context.Background(), dbPath(t), spec)
			require.Error(t, err)
			assert.Nil(t, s)
		})
	}
}

func TestUnknownResource(t *testing.T) {
	s := openStore(t, dbPath(t), repoSpec())
	_, ok := s.Resource("nothing")
	assert.False(t, ok)
}

// A dead database must fail loudly on every path, never report a write that
// did not happen.
func TestOperationsOnAClosedStoreFail(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())
	r, ok := s.Resource("repo")
	require.True(t, ok)
	require.NoError(t, s.Close())

	row := Row{"owner": "wow", "repo": "a", "visibility": "public", "stars": int64(1)}
	at := time.Unix(1700000000, 0)

	assert.Error(t, s.Put(ctx, r, row, at))
	assert.Error(t, s.PutMany(ctx, r, []Row{row}, at))
	_, err := s.Get(ctx, r, map[string]string{"owner": "wow", "repo": "a"})
	assert.Error(t, err)
	_, err = s.List(ctx, r, nil)
	assert.Error(t, err)
	_, err = s.Delete(ctx, r, map[string]string{"owner": "wow"})
	assert.Error(t, err)

	_, err = s.Meta(ctx, "fingerprint")
	assert.Error(t, err)
	assert.Error(t, s.SetMeta(ctx, "k", "v"))
	_, err = s.Freshness(ctx, "repo", "wow/a")
	assert.Error(t, err)
	assert.Error(t, s.MarkFetching(ctx, "repo", "wow/a"))
	assert.Error(t, s.MarkError(ctx, "repo", "wow/a", "boom", at))
	assert.Error(t, s.RecordFetched(ctx, Freshness{Kind: "repo", Key: "wow/a", State: "fresh"}))
	_, err = s.DeleteFreshness(ctx, "repo", "wow/a")
	assert.Error(t, err)
	_, _, err = s.Watermark(ctx, "repo:wow/a")
	assert.Error(t, err)
	_, err = s.ApplyWatermark(ctx, "repo:wow/a", at)
	assert.Error(t, err)
	_, err = s.PruneWatermarks(ctx, at)
	assert.Error(t, err)
	_, err = s.HasGrant(ctx, "user:1", "repo", "wow/a", at)
	assert.Error(t, err)
	assert.Error(t, s.RecordGrant(ctx, Grant{Principal: "user:1", Resource: "repo", Key: "wow/a", ExpiresAt: at}))
	assert.Error(t, s.ReplaceGrants(ctx, "user:1", "repo", "list_sync", []string{"wow/a"}, at))
	_, err = s.RevokeGrant(ctx, "user:1", "repo", "wow/a")
	assert.Error(t, err)
	_, err = s.PruneGrants(ctx, at)
	assert.Error(t, err)
	_, _, err = s.Denial(ctx, "user:1", "repo", "wow/a", at)
	assert.Error(t, err)
	assert.Error(t, s.RecordDenial(ctx, "user:1", "repo", "wow/a", 404, at))
	_, err = s.PruneDenials(ctx, at)
	assert.Error(t, err)
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, dbPath(t), repoSpec())

	v, err := s.Meta(ctx, "nothing")
	require.NoError(t, err)
	assert.Empty(t, v, "a missing key reads as empty, not as an error")

	require.NoError(t, s.SetMeta(ctx, "spec_name", "test"))
	require.NoError(t, s.SetMeta(ctx, "spec_name", "test2"))
	v, err = s.Meta(ctx, "spec_name")
	require.NoError(t, err)
	assert.Equal(t, "test2", v)
}
