
CREATE TABLE IF NOT EXISTS subscription (
	id           TEXT PRIMARY KEY,
	principal    TEXT NOT NULL,
	url          TEXT NOT NULL,
	secret       TEXT NOT NULL,
	events       TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	last_ok      INTEGER,
	failures     INTEGER NOT NULL DEFAULT 0,
	disabled     INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS subscription_principal ON subscription (principal);
CREATE TABLE IF NOT EXISTS subscription_import (
	source          TEXT NOT NULL,
	source_id       TEXT NOT NULL,
	subscription_id TEXT NOT NULL,
	imported_at     INTEGER NOT NULL,
	PRIMARY KEY (source, source_id)
);
