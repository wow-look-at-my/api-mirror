-- name: GetFreshness :one
SELECT * FROM mirror_freshness WHERE kind = ? AND key = ?;

-- A fetch that succeeded clears the error and the backoff with it. Leaving
-- either behind holds off the next fetch on a failure that already healed.
-- name: RecordFetched :exec
INSERT INTO mirror_freshness (kind, key, fetched_at, changed_at, etag, expires_at, state, error, retry_after)
VALUES (?, ?, ?, ?, ?, ?, ?, '', NULL)
ON CONFLICT (kind, key) DO UPDATE SET
	fetched_at  = excluded.fetched_at,
	changed_at  = excluded.changed_at,
	etag        = excluded.etag,
	expires_at  = excluded.expires_at,
	state       = excluded.state,
	error       = '',
	retry_after = NULL;

-- Every mark upserts. A first fetch has no row yet, and an UPDATE would report
-- success while recording nothing.
-- name: MarkFetching :exec
INSERT INTO mirror_freshness (kind, key, state) VALUES (?, ?, 'fetching')
ON CONFLICT (kind, key) DO UPDATE SET state = 'fetching';

-- name: MarkError :exec
INSERT INTO mirror_freshness (kind, key, state, error, retry_after)
VALUES (?, ?, 'error', ?, ?)
ON CONFLICT (kind, key) DO UPDATE SET
	state       = 'error',
	error       = excluded.error,
	retry_after = excluded.retry_after;

-- name: DeleteFreshness :execrows
DELETE FROM mirror_freshness WHERE kind = ? AND key = ?;
