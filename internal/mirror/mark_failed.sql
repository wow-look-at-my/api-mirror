UPDATE subscription SET failures = failures + 1, last_error = ?, last_failure = ?,
		 disabled_reason = CASE WHEN failures + 1 >= ? AND disabled = 0
			THEN 'disabled after ' || (failures + 1) || ' consecutive delivery failures' ELSE disabled_reason END,
		 disabled = CASE WHEN failures + 1 >= ? THEN 1 ELSE disabled END WHERE id = ?