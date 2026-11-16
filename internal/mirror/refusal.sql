CREATE TABLE mirror_refusal (
	kind TEXT NOT NULL,
	key  TEXT NOT NULL,
	body TEXT NOT NULL,
	PRIMARY KEY (kind, key)
);
