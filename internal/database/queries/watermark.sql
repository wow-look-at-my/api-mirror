-- name: GetWatermark :one
SELECT applied_at FROM mirror_watermark WHERE subject = ?;

-- Applies only when the incoming view is at least as new as the stored one,
-- and reports through the affected-row count whether it did.
--
-- Equality MUST apply. A clock here is a second, and two genuinely distinct
-- views of one subject land inside the same second all the time; refusing on
-- equality drops the second one.
-- name: ApplyWatermark :execrows
INSERT INTO mirror_watermark (subject, applied_at) VALUES (?, ?)
ON CONFLICT (subject) DO UPDATE SET applied_at = excluded.applied_at
WHERE excluded.applied_at >= mirror_watermark.applied_at;

-- name: PruneWatermarks :execrows
DELETE FROM mirror_watermark WHERE applied_at < ?;
