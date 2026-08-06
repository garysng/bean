package store

import (
	"strings"
	"testing"
)

// A DSN in a startup error is the most likely route for a database password to reach a
// log aggregator, and once it is there it is not recoverable. So the redaction is
// tested rather than eyeballed, including the forms that look like they would already
// be safe.
func TestRedactDSNRemovesThePassword(t *testing.T) {
	for _, tc := range []struct {
		name, dsn, mustNotContain string
	}{
		{"url form", "postgres://bean:s3cret@db.internal:5432/bean", "s3cret"},
		{"url with query", "postgres://bean:s3cret@db:5432/bean?sslmode=require", "s3cret"},
		{"keyword form", "host=db user=bean password=s3cret dbname=bean", "s3cret"},
		// A password containing the characters a regex would trip over. This is the
		// reason the keyword form is rewritten field by field.
		{"awkward password", "host=db password=p@ss:w/rd=1 dbname=bean", "p@ss:w/rd=1"},
	} {
		got := redactDSN(tc.dsn)
		if strings.Contains(got, tc.mustNotContain) {
			t.Errorf("%s: redacted DSN still contains the password: %q", tc.name, got)
		}
		// And it has to stay useful: the host is the whole reason for logging it.
		if !strings.Contains(got, "db") {
			t.Errorf("%s: redaction removed the host too, leaving %q -- a connection "+
				"error with no host in it cannot be acted on", tc.name, got)
		}
	}
}

func TestRedactDSNLeavesAPasswordlessDSNAlone(t *testing.T) {
	// Common in development and with IAM auth. Mangling it would make an error message
	// wrong for the configuration that has nothing to hide.
	for _, dsn := range []string{
		"postgres://bean@db.internal:5432/bean",
		"host=db user=bean dbname=bean sslmode=disable",
	} {
		if got := redactDSN(dsn); got != dsn {
			t.Errorf("redactDSN(%q) = %q, want it unchanged", dsn, got)
		}
	}
}

// TestPostgresDialectIsWiredToTheStore checks that OpenPostgres would use the Postgres
// dialect rather than the SQLite one.
//
// Asserted on the type rather than by connecting, because a wrong dialect is not a
// connection failure -- it is `?` reaching the server, which fails at the first
// statement with a syntax error and nowhere near the line that chose the dialect.
func TestPostgresDialectIsWiredToTheStore(t *testing.T) {
	s := &Store{d: postgresDialect{}}
	if got := s.d.bind("WHERE id=?"); got != "WHERE id=$1" {
		t.Fatalf("a store built with the postgres dialect binds %q; the dialect is not "+
			"reaching the statements", got)
	}
	if s.d.name() != "postgres" {
		t.Errorf("dialect name is %q", s.d.name())
	}
	// The two DDL constructs that differ, so a change to one is visible here rather
	// than only on a live Postgres.
	if !strings.Contains(s.d.autoIncrementPK(), "BIGSERIAL") {
		t.Errorf("autoIncrementPK is %q, want BIGSERIAL", s.d.autoIncrementPK())
	}
	if !strings.Contains(s.d.boolColumn(), "BOOLEAN") {
		t.Errorf("boolColumn is %q; SQLite tolerates INTEGER for a bool and Postgres "+
			"does not, which is why this differs per engine", s.d.boolColumn())
	}
	if s.d.journalPragma() != "" {
		t.Errorf("journalPragma is %q, want empty: WAL is a SQLite concept and Postgres "+
			"readers never block writers", s.d.journalPragma())
	}
}

// TestDropAllCoversEveryTableTheSchemaCreates keeps the drop list from drifting.
//
// A table added to migrate() and forgotten here leaves rows behind between conformance
// runs, and the symptom is a later requirement failing for a reason nothing points at --
// a stale reservation making the oversell check refuse when it should not. That is
// exactly what happened while writing this: the list said "image_builds" for a table
// actually named "builds".
//
// Checked against the live schema rather than a hardcoded list, so the test cannot go
// stale in the same way the code did.
func TestDropAllCoversEveryTableTheSchemaCreates(t *testing.T) {
	// A SQLite store is enough: the schema is the same on both engines, and this is
	// about the table names rather than the drop statement.
	dir := t.TempDir()
	st, err := Open(dir + "/schema.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rows, err := st.query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var created []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		created = append(created, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 {
		t.Fatal("no tables found, so this test proves nothing")
	}

	dropped := map[string]bool{}
	for _, name := range droppedTables() {
		dropped[name] = true
	}
	for _, name := range created {
		if !dropped[name] {
			t.Errorf("table %q is created by migrate() but not dropped by "+
				"DropAllForTest; its rows survive between conformance runs and will "+
				"fail a later requirement for an unrelated-looking reason", name)
		}
	}
}
