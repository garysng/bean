package store

import (
	"fmt"
	"regexp"
	"strings"
)

// A second engine is a dialect, not a second store.
//
// Surveyed before choosing: of 1424 lines, what differs between SQLite and Postgres is
// 103 `?` placeholders, one AUTOINCREMENT, and one column declared INTEGER but scanned
// into a Go bool. All eight ON CONFLICT clauses port unchanged.
//
// That survey was right about the shape and wrong about the inventory, which is worth
// stating here because the corrected count is the argument for the conformance suite.
// Running migrate() against a real Postgres found three more differences the reading had
// missed: `secret BLOB`, which Postgres has no type for; ADD COLUMN idempotency, which
// only one engine can express in SQL; and INTEGER, which is 64 bits in SQLite and 32 in
// Postgres, so every millisecond timestamp overflowed. The first two are constructs a
// grep could have found with a better list. The third could not be read at all -- the
// spelling is identical and the meaning is not.
//
// So there is one set of statements and a translation, rather than two implementations
// of 39 methods each. The alternative was rejected on the evidence rather than on
// taste: two bodies of SQL that must agree, with a conformance suite that can only tell
// you afterwards which one drifted, is a worse position than one body with a mechanical
// rewrite. The store's own history is the argument -- the mutex was uniform across 37
// methods and still wrong in exactly two of them, and duplication is how that kind of
// thing survives review.
//
// What a dialect is NOT allowed to do: change semantics. Each translation below is
// syntax only, so a statement's meaning is decided once and a new engine cannot quietly
// weaken a condition. If a future engine needs different semantics for a statement,
// that statement belongs in its own method with its own conformance requirement, not in
// a dialect switch.

// dialect is the syntax a particular engine expects.
type dialect interface {
	// name identifies the engine in errors and logs.
	name() string

	// bind rewrites `?` placeholders into whatever the driver expects. SQLite takes
	// `?`; pq takes `$1`, `$2`.
	bind(query string) string

	// autoIncrementPK renders the primary-key clause for a table whose ids the
	// database generates. This is the only DDL that genuinely differs: SQLite spells
	// it `INTEGER PRIMARY KEY AUTOINCREMENT`, Postgres `BIGSERIAL PRIMARY KEY`.
	autoIncrementPK() string

	// boolColumn renders a column that holds a boolean.
	//
	// SQLite has no boolean type and accepts INTEGER for one, which is why nvme_cache
	// was declared INTEGER and scanned into a Go bool without complaint. Postgres
	// refuses that scan, and the failure would be at read time on a column nothing
	// reads on the hot path -- so it is spelled correctly per engine instead.
	boolColumn() string

	// ddl rewrites a schema statement for the engine. Applied to CREATE TABLE and to
	// ALTER TABLE ADD COLUMN, not to queries -- nothing in a WHERE clause names a type.
	//
	// It exists for one word. SQLite's INTEGER holds 64 bits; Postgres's holds 32, and
	// every timestamp column in this schema stores Unix milliseconds, which passed 2^31
	// in 1970 plus 25 days. The measured failure was `pq: value "1785984748685" is out
	// of range for type integer` on five of seven requirements.
	//
	// Worth recording how this got missed: the up-front survey checked that timestamps
	// were stored as integer milliseconds rather than as a date type, and concluded they
	// were portable. They are -- the representation is fine and the column width is not.
	// Reading SQL as text finds the constructs that differ, never the ones that are
	// spelled identically and mean different things.
	//
	// Rewritten mechanically rather than by declaring 24 columns through a helper, for
	// the reason the whole dialect layer exists: a per-column decision is a per-column
	// opportunity to pick the narrow one, and the next column added would be INTEGER
	// again because that is what the 24 above it say.
	ddl(statement string) string

	// addColumn renders an idempotent ALTER TABLE ADD COLUMN, and isDuplicateColumn
	// reports whether an error from it means the column was already there.
	//
	// The pair exists because only one engine can express the intent in SQL. Postgres has
	// ADD COLUMN IF NOT EXISTS, so its addColumn is idempotent and its isDuplicateColumn
	// is always false -- any error is a real one. SQLite has no such clause, so its
	// addColumn is a plain ALTER and the duplicate case has to be recognised by message
	// text.
	//
	// Kept as two methods rather than a single "add if missing" helper because the second
	// is what a caller forgets: a text match written for SQLite's wording, applied to
	// Postgres, either swallows unrelated errors or lets a duplicate abort startup.
	addColumn(table, definition string) string
	isDuplicateColumn(err error) bool

	// blobColumn renders a column that holds opaque bytes.
	//
	// One column needs this: registry_credentials.secret, which holds ciphertext the
	// store never decrypts. SQLite spells it BLOB, Postgres has no such type and
	// rejects the schema outright with `type "blob" does not exist`.
	//
	// This one is worth noting because it is the difference the up-front measurement
	// missed. Counting placeholders and grepping for AUTOINCREMENT found 103 binds and
	// two DDL constructs and concluded the dialect was small; BLOB appeared once, in a
	// table no unit test exercises, and only running migrate() against a real Postgres
	// surfaced it. The measurement was right about the shape and wrong about the
	// inventory -- which is the argument for the conformance suite rather than against
	// the dialect layer.
	blobColumn() string

	// journalPragma is the statement that makes concurrent readers safe, or "" for an
	// engine where they always are.
	//
	// SQLite needs WAL or a reader and the writer block each other; bean-proxy reads
	// this database while the control plane writes it, so without WAL the proxy stalls
	// on every create. Postgres needs nothing: MVCC means readers never block writers.
	journalPragma() string
}

// sqliteDialect is the engine bean ships with.
type sqliteDialect struct{}

func (sqliteDialect) name() string            { return "sqlite" }
func (sqliteDialect) bind(q string) string    { return q }
func (sqliteDialect) autoIncrementPK() string { return "INTEGER PRIMARY KEY AUTOINCREMENT" }
func (sqliteDialect) boolColumn() string      { return "INTEGER NOT NULL DEFAULT 0" }
func (sqliteDialect) blobColumn() string      { return "BLOB NOT NULL" }

// Identity: the schema is written in SQLite's spelling, so there is nothing to rewrite.
func (sqliteDialect) ddl(stmt string) string { return stmt }

func (sqliteDialect) addColumn(table, definition string) string {
	return "ALTER TABLE " + table + " ADD COLUMN " + definition
}

// Matched on text because SQLite reports this through a generic error. Narrow on purpose:
// "duplicate column name" is the only wording that means the column is already present,
// and anything broader would hide a genuinely failed migration behind a silent skip.
func (sqliteDialect) isDuplicateColumn(err error) bool {
	return strings.Contains(err.Error(), "duplicate column name")
}
func (sqliteDialect) journalPragma() string { return "PRAGMA journal_mode=WAL" }

// postgresDialect targets Postgres 12 or later.
//
// 12 is the floor because these statements use no feature newer than that, not because
// anything was tested against 11 and rejected -- worth saying plainly so nobody reads
// it as a verified minimum.
type postgresDialect struct{}

func (postgresDialect) name() string { return "postgres" }

// bind converts `?` to `$1`, `$2`, ... in order.
//
// A scan rather than strings.Replace, because a `?` inside a string literal must not be
// rewritten. None of this package's statements contain one today, and that is exactly
// the kind of fact that stops being true without anyone noticing -- so the parser
// tracks quoting instead of relying on it.
func (postgresDialect) bind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'' && !inDouble:
			// Doubled quotes ('') are an escaped quote inside a literal, not the end of
			// one. Skipping both keeps the state correct.
			if inSingle && i+1 < len(q) && q[i+1] == '\'' {
				b.WriteByte(c)
				b.WriteByte(q[i+1])
				i++
				continue
			}
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '?' && !inSingle && !inDouble:
			n++
			b.WriteString("$")
			b.WriteString(fmt.Sprint(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (postgresDialect) autoIncrementPK() string { return "BIGSERIAL PRIMARY KEY" }
func (postgresDialect) boolColumn() string      { return "BOOLEAN NOT NULL DEFAULT FALSE" }

// BYTEA rather than TEXT: SecretCiphertext is []byte, and lib/pq maps []byte to BYTEA
// natively. TEXT would round-trip through an encoding and corrupt any ciphertext byte that
// is not valid UTF-8.
func (postgresDialect) blobColumn() string { return "BYTEA NOT NULL" }

// integerType matches the type keyword only where it stands alone, so a column that
// happens to contain the letters is untouched. Anchored on word boundaries rather than
// replaced by substring for the same reason bind() tracks quotes: a rewrite that is right
// about the common case and wrong about one identifier produces a schema that builds and
// then fails on one table.
var integerType = regexp.MustCompile(`\bINTEGER\b`)

// BIGINT for every INTEGER. Widening is safe in a way narrowing would not be: every
// scan target in this package is a Go int64 or int, so no column loses range, and the
// values that overflowed 32 bits are timestamps that will keep growing.
//
// Runs after autoIncrementPK, boolColumn and blobColumn have been interpolated, and none
// of their Postgres spellings (BIGSERIAL, BOOLEAN, BYTEA) contains the word -- so this
// cannot corrupt them.
func (postgresDialect) ddl(stmt string) string {
	return integerType.ReplaceAllString(stmt, "BIGINT")
}

func (postgresDialect) addColumn(table, definition string) string {
	return "ALTER TABLE " + table + " ADD COLUMN IF NOT EXISTS " + definition
}

// Always false: the IF NOT EXISTS above means a duplicate is not an error here, so any
// error that does arrive is a real failure and must not be swallowed.
func (postgresDialect) isDuplicateColumn(error) bool { return false }
func (postgresDialect) journalPragma() string        { return "" }
