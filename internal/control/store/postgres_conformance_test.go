package store_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/garysng/bean/internal/control/store"
	"github.com/garysng/bean/internal/control/store/storetest"
)

// The Postgres store against the same conformance suite SQLite runs.
//
// This is the test that decides whether the Postgres support is real. The dialect layer
// and the compile-time assertions between them prove that the statements are rewritten
// and the methods exist; neither can show that a reference count survives concurrent
// acquires on a different engine, or that Reserve refuses to oversell when the row locks
// behave differently. Only running the requirements does that.
//
// Skipped without BEAN_TEST_POSTGRES_DSN rather than spinning up a container, because a
// test that silently starts Docker is a test that behaves differently on a laptop and in
// CI. hack/postgres-conformance.sh brings one up and sets the variable.
//
// It also means the skip is honest: if this suite has never run against a real Postgres,
// the output says so instead of reporting a pass earned by SQLite.
func TestPostgresSatisfiesTheContract(t *testing.T) {
	dsn := os.Getenv("BEAN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BEAN_TEST_POSTGRES_DSN to run the contract against Postgres " +
			"(see hack/postgres-conformance.sh)")
	}

	// Each handle is a separate connection to the same database, which is what makes
	// the concurrency requirements meaningful: a lock inside one handle would serialise
	// the callers and the races could not be reproduced.
	//
	// The schema is dropped first so a rerun does not inherit rows from the last one.
	// Postgres has no equivalent of deleting a SQLite file, and a stale reservation from
	// a previous run would fail the oversell requirement for the wrong reason.
	admin, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := admin.DropAllForTest(); err != nil {
		admin.Close()
		t.Fatalf("reset schema: %v", err)
	}
	admin.Close()

	opened := 0
	storetest.Run(t, func(t *testing.T) storetest.Store {
		st, err := store.OpenPostgres(dsn)
		if err != nil {
			t.Fatalf("open postgres handle: %v", err)
		}
		opened++
		t.Cleanup(func() { _ = st.Close() })
		return st
	})

	if opened == 0 {
		t.Fatal("the suite opened no handles, so nothing was actually exercised")
	}
	fmt.Printf("postgres conformance: %d handles opened\n", opened)
}
