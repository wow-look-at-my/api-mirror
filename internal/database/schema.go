// Package database holds the engine's own tables: the SQL that declares them,
// the typed queries sqlc generates from that SQL, and the fingerprint that
// decides when a cache is rebuilt.
//
// These five tables are the same for every spec, so they are fixed at compile
// time and their access is generated. A resource table is declared by the
// spec at run time, which no generator can type; it is derived and driven
// dynamically instead.
package database

import _ "embed"

// Schema is the engine's DDL. A spec appends its derived resource tables to it
// to make the whole schema.
//
//go:embed schema.sql
var Schema string
