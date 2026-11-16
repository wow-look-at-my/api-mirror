INSERT INTO %s (%s, mirror_written_at) VALUES (%s)
ON CONFLICT (%s) DO UPDATE SET %s
WHERE excluded.%s IS NULL OR %s.%s IS NULL OR excluded.%s >= %s.%s