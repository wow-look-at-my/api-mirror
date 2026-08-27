-- name: GetFreshness :one
SELECT * FROM mirror_freshness WHERE kind = ? AND key = ?;

-- A fetch that succeeded clears the error and the backoff with it. Leaving
-- either behind holds off the next fetch on a failure that already healed.
-- name: RecordFetched :exec
INSERT INTO mirror_freshness (kind, key, fetched_at, changed_at, etag, expires_at, state, status, error, retry_after)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', NULL)
ON CONFLICT (kind, key) DO UPDATE SET
	fetched_at  = excluded.fetched_at,
	changed_at  = excluded.changed_at,
	etag        = excluded.etag,
	expires_at  = excluded.expires_at,
	state       = excluded.state,
	status      = excluded.status,
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

-- The sweep asks for the keys of one kind that have aged out, oldest first.
-- A row still inside its error backoff is included: the sweep is a deliberate
-- refresh, and a deliberate refresh is exactly what the backoff does not hold
-- off, having no consumer waiting on it.
-- name: ListStaleKeys :many
SELECT key, etag, state, expires_at FROM mirror_freshness
WHERE kind = ? AND (expires_at IS NULL OR expires_at <= ?)
ORDER BY expires_at LIMIT ?;

-- The dashboard reports what one kind holds. Keys are what an operator reads,
-- so the listing is by key and not a bare count.
-- name: ListFreshnessByKind :many
SELECT * FROM mirror_freshness WHERE kind = ? ORDER BY key LIMIT ?;

-- name: CountFreshnessByKind :many
SELECT kind, COUNT(*) AS rows_held,
  SUM(CASE WHEN state = 'error' THEN 1 ELSE 0 END) AS errored,
  MAX(COALESCE(fetched_at, 0)) AS newest
FROM mirror_freshness GROUP BY kind ORDER BY kind;
