package store_test

import (
	"path/filepath"
	"testing"

	"github.com/garysng/bean/internal/control/store"
	"github.com/garysng/bean/internal/control/store/storetest"
)

// The SQLite store against the conformance suite.
//
// An external test package (store_test) rather than the internal one, deliberately: this
// exercises the store exactly as another implementation would, through its exported
// surface. If it needed unexported access to pass, that would be worth knowing.
func TestSQLiteSatisfiesTheContract(t *testing.T) {
	// One database file, a fresh handle per call. Separate handles are the point -- a
	// process-local lock inside one handle would serialise the callers and make the
	// concurrency requirements unfalsifiable.
	path := filepath.Join(t.TempDir(), "conformance.db")
	storetest.Run(t, func(t *testing.T) storetest.Store {
		st, err := store.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	})
}
