-- Every read filters on expiry, so an expired row is unreadable the moment it
-- expires rather than the moment a prune gets to it.
-- name: CountLiveGrants :one
SELECT COUNT(*) FROM mirror_grant
WHERE principal = ? AND resource = ? AND key = ? AND expires_at > ?;

-- name: RecordGrant :exec
INSERT INTO mirror_grant (principal, resource, key, source, expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (principal, resource, key) DO UPDATE SET
	source     = excluded.source,
	expires_at = excluded.expires_at;

-- Replace-sync: a list answer states the whole set this source knows about, so
-- what it no longer names is gone rather than merged with.
-- name: DeleteGrantsBySource :execrows
DELETE FROM mirror_grant WHERE principal = ? AND resource = ? AND source = ?;

-- name: RevokeGrant :execrows
DELETE FROM mirror_grant WHERE principal = ? AND resource = ? AND key = ?;

-- name: PruneGrants :execrows
DELETE FROM mirror_grant WHERE expires_at <= ?;

-- name: GetDenial :one
SELECT status FROM mirror_deny
WHERE principal = ? AND resource = ? AND key = ? AND expires_at > ?;

-- name: RecordDenial :exec
INSERT INTO mirror_deny (principal, resource, key, status, expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (principal, resource, key) DO UPDATE SET
	status     = excluded.status,
	expires_at = excluded.expires_at;

-- name: DeleteDenial :execrows
DELETE FROM mirror_deny WHERE principal = ? AND resource = ? AND key = ?;

-- name: PruneDenials :execrows
DELETE FROM mirror_deny WHERE expires_at <= ?;
