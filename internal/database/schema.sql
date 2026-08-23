-- The engine's own tables. Every spec derives the same five, so they take the
-- mirror_ prefix and a resource may not.
--
-- This file is sqlc's input. These tables are fixed at compile time, so their
-- queries are generated Go. A resource table is declared by the spec at run
-- time and is derived instead; nothing about it can live here.
--
-- Changing this file rebuilds the cache. fingerprint.go hashes it with the
-- comments scrubbed, together with the DDL the spec derives, so editing a
-- table nukes and editing a comment does not.

-- mirror_meta records the fingerprint the file on disk was built from.
CREATE TABLE mirror_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- mirror_freshness is one row per fetched resource key: when the answer
-- arrived, when it last changed, and the backoff a failing upstream earns.
-- A NULL time means the moment never happened.
CREATE TABLE mirror_freshness (
	kind        TEXT NOT NULL,
	key         TEXT NOT NULL,
	fetched_at  INTEGER,
	changed_at  INTEGER,
	etag        TEXT NOT NULL DEFAULT '',
	expires_at  INTEGER,
	state       TEXT NOT NULL DEFAULT 'unknown',
	error       TEXT NOT NULL DEFAULT '',
	retry_after INTEGER,
	PRIMARY KEY (kind, key)
);

-- mirror_watermark is the newest view already applied for one subject. A
-- delivery older than this restates superseded state, so the engine drops it.
CREATE TABLE mirror_watermark (
	subject    TEXT PRIMARY KEY,
	applied_at INTEGER NOT NULL
);

-- mirror_grant and mirror_deny are the ONLY tables keyed by a principal. That
-- asymmetry is the whole security model: what is stored is global, what is
-- proven is per-caller.
--
-- A grant is proof that this principal read this resource upstream. It expires,
-- so proof is re-earned rather than inherited.
CREATE TABLE mirror_grant (
	principal  TEXT NOT NULL,
	resource   TEXT NOT NULL,
	key        TEXT NOT NULL,
	source     TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (principal, resource, key)
);
CREATE INDEX mirror_grant_expiry ON mirror_grant (expires_at);

-- A denial is an authoritative refusal from upstream, cached briefly so a
-- caller who cannot read a resource does not re-ask on every request. Only an
-- authoritative status lands here; a transient failure is never cached.
CREATE TABLE mirror_deny (
	principal  TEXT NOT NULL,
	resource   TEXT NOT NULL,
	key        TEXT NOT NULL,
	status     INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	PRIMARY KEY (principal, resource, key)
);
CREATE INDEX mirror_deny_expiry ON mirror_deny (expires_at);
