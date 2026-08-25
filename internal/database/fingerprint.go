package database

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Fingerprint identifies a whole schema: this package's fixed tables plus the
func Fingerprint(ddl string) string {
	sum := sha256.Sum256([]byte(Scrub(ddl)))
	return hex.EncodeToString(sum[:])
}

// Scrub reduces DDL to the tables it builds: comments dropped, formatting
// normalized. Normalizing keeps the decision honest in both directions.
// Hashing raw text rebuilds a fleet's cache over a reworded comment. Dropping
// too much is worse: two different schemas that scrub alike deploy a new
// column against an old table.
//
// So the bytes that are load-bearing stay exactly as written. A quoted run is
// copied verbatim, spacing included, and a gap between two ordinary tokens
// becomes one space.
func Scrub(sql string) string {
	var out scrubbed
	for i := 0; i < len(sql); {
		switch c := sql[i]; {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.gap()

		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i += 2
			for i < len(sql) && !(sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/') {
				i++
			}
			i = min(i+2, len(sql))
			out.gap()

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			out.gap()

		case c == '\'' || c == '"' || c == '`':
			end := endOfQuoted(sql, i, c)
			out.write(sql[i:end])
			i = end

		case c == '[':
			end := endOfQuoted(sql, i, ']')
			out.write(sql[i:end])
			i = end

		default:
			out.write(sql[i : i+1])
			i++
		}
	}
	return out.b.String()
}

// scrubbed accumulates the normalized text. It holds each gap open until it
// knows what follows.
type scrubbed struct {
	b       strings.Builder
	last    byte
	pending bool
}

func (s *scrubbed) gap() { s.pending = s.b.Len() > 0 }

func (s *scrubbed) write(tok string) {
	if s.pending {
		if !selfDelimiting(s.last) && !selfDelimiting(tok[0]) {
			s.b.WriteByte(' ')
		}
		s.pending = false
	}
	s.b.WriteString(tok)
	s.last = tok[len(tok)-1]
}

// selfDelimiting reports whether a character separates tokens by itself, so
func selfDelimiting(c byte) bool {
	return c == '(' || c == ')' || c == ',' || c == ';'
}

// endOfQuoted returns the index just past the quoted run opening at
// sql[start]. A doubled closing quote is an escaped one. An unterminated run
func endOfQuoted(sql string, start int, closer byte) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] != closer {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == closer {
			i++
			continue
		}
		return i + 1
	}
	return len(sql)
}
