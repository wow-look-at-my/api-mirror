CREATE TABLE mirror_list_member (
	kind     TEXT    NOT NULL,
	list_key TEXT    NOT NULL,
	position INTEGER NOT NULL,
	row_key  TEXT    NOT NULL,
	PRIMARY KEY (kind, list_key, position)
);
