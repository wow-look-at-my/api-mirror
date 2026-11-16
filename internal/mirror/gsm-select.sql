SELECT id, principal, url, secret, repo_filters, event_filters, active,
	consecutive_failures, disabled_reason, created_at, updated_at,
	last_success_at, last_failure_at, last_error
	FROM subscriptions ORDER BY created_at, id