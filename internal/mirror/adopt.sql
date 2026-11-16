INSERT INTO subscription (id, principal, url, secret, events, scopes, created_at, updated_at,
			last_ok, last_failure, failures, disabled, last_error, disabled_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)