-- name: GetMeta :one
SELECT value FROM mirror_meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO mirror_meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;
