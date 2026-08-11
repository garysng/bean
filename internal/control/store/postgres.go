package store

import (
	"database/sql"
	"fmt"
	"strings"

	// Registered for its side effect, as with the SQLite driver above. lib/pq rather
	// than pgx: this package uses nothing beyond database/sql, so the richer driver's
	// advantage -- its native interface -- is one nothing here would call.
	_ "github.com/lib/pq"
)

// OpenPostgres opens the store against Postgres.
//
// The same 39 methods and the same statements as SQLite; what differs is the dialect
// (see dialect.go), which was measured at 103 placeholders plus two DDL constructs
// rather than assumed to be large.
//
// Why this matters beyond running somewhere else: SQLite is one file, so two
// control-plane replicas cannot share it, and that is the actual limit on running more
// than one bean-api. The interfaces and the conformance suite came first for that
// reason -- an engine swap under methods whose atomicity lived in a process mutex would
// have promised multi-replica safety while delivering lost updates.
//
// dsn is a libpq connection string or URL, e.g.
// "postgres://bean:secret@db.internal:5432/bean?sslmode=require".
func OpenPostgres(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	// Unlike SQLite, which is capped at one connection because it has a single writer.
	// Postgres has none of that constraint, and a control plane serving concurrent
	// requests through one connection would serialise itself for no reason.
	//
	// 25 is a starting point rather than a measurement: it is well under the default
	// max_connections of 100, leaving room for the other replicas that are the whole
	// point of running Postgres. If this is ever tuned it should be from observed pool
	// waits, and the comment should say so.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)

	// Checked here rather than at first query. sql.Open does not connect, so a wrong
	// DSN or an unreachable database would otherwise surface as a failed create long
	// after startup, on a node that reported itself healthy.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: cannot reach %s: %w", redactDSN(dsn), err)
	}

	s := &Store{db: db, d: postgresDialect{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DropAllForTest removes every table this package creates.
//
// It exists because the conformance suite needs a clean database and Postgres has no
// equivalent of deleting a SQLite file. A stale reservation from a previous run fails the
// oversell requirement for the wrong reason, which is worse than not running it.
//
// The name carries ForTest because there is no way to make this safe by construction --
// a method that drops tables is dangerous whatever it is called, so the name is the
// warning. Two things narrow it: it refuses on any engine but Postgres, since SQLite
// deployments have no reason to reach for it and the real database people run locally is
// SQLite; and it names only this package's tables rather than dropping a schema, so it
// cannot take anything a shared database happens to hold alongside.
func (s *Store) DropAllForTest() error {
	if s.d.name() != "postgres" {
		return fmt.Errorf("DropAllForTest refuses on %s: delete the file instead",
			s.d.name())
	}
	for _, table := range droppedTables() {
		if _, err := s.db.Exec("DROP TABLE IF EXISTS " + table + " CASCADE"); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	return s.migrate()
}

// droppedTables names every table migrate() creates.
//
// Named individually rather than `DROP SCHEMA public CASCADE`, which would also remove
// whatever else shares the database. Separate from DropAllForTest so a test can check the
// list against the live schema: the first version said "image_builds" for what is actually
// "builds", which would have left rows behind and failed a later requirement for a reason
// nothing pointed at.
func droppedTables() []string {
	return []string{
		"reservations", "nodes", "prewarm_jobs", "templates", "snapshots", "events",
		"sandboxes", "registry_credentials", "builds",
	}
}

// redactDSN removes the password so a connection failure can be logged.
//
// A DSN in a startup error is the single most likely way for a database password to
// reach a log aggregator, and the error is useless without the host -- so the host is
// kept and the secret is not.
func redactDSN(dsn string) string {
	// URL form: scheme://user:password@host/...
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			creds := rest[:at]
			if colon := strings.Index(creds, ":"); colon >= 0 {
				return dsn[:i+3] + creds[:colon] + ":***@" + rest[at+1:]
			}
		}
		return dsn
	}
	// Keyword form: host=... password=... -- rewritten field by field, because a
	// password may legitimately contain characters that make a regex wrong.
	fields := strings.Fields(dsn)
	for i, f := range fields {
		if strings.HasPrefix(f, "password=") {
			fields[i] = "password=***"
		}
	}
	return strings.Join(fields, " ")
}
