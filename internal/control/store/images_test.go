package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func img(ref, owner string, source ImageSource) *Image {
	return &Image{
		Ref: ref, State: ImageReady, Source: source, Owner: owner,
		CreatedAt: time.Now(),
	}
}

func TestListImagesUnfilteredReturnsEverything(t *testing.T) {
	st := openTestStore(t)
	for _, in := range []*Image{
		img("python:3.12", "", ImageImported),
		img("team-a/app:v1", "user-a", ImageBuilt),
		img("team-b/app:v1", "user-b", ImageBuilt),
	} {
		if err := st.PutImage(in); err != nil {
			t.Fatal(err)
		}
	}
	// An empty owner is the operator's view, and the behaviour of every
	// deployment that configures no identity.
	got, err := st.ListImages("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("images = %d, want 3", len(got))
	}
}

func TestListImagesScopesToOwnerPlusUnowned(t *testing.T) {
	st := openTestStore(t)
	for _, in := range []*Image{
		img("python:3.12", "", ImageImported),
		img("team-a/app:v1", "user-a", ImageBuilt),
		img("team-b/app:v1", "user-b", ImageBuilt),
	} {
		if err := st.PutImage(in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListImages("user-a")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]bool{}
	for _, i := range got {
		refs[i.Ref] = true
	}
	if !refs["team-a/app:v1"] {
		t.Error("owner cannot see their own image")
	}
	// Unowned means shared, not hidden: an upgraded deployment must still show
	// the base images it can run.
	if !refs["python:3.12"] {
		t.Error("unowned image hidden from owner")
	}
	if refs["team-b/app:v1"] {
		t.Error("another owner's image leaked into the listing")
	}
}

func TestPutImagePreservesOwnerAcrossReload(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutImage(img("mine:v1", "user-a", ImageBuilt)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetImage("mine:v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "user-a" {
		t.Errorf("owner = %q, want user-a", got.Owner)
	}
	if got.Source != ImageBuilt {
		t.Errorf("source = %q, want built", got.Source)
	}
}

// TestOwnerColumnAddedToExistingDatabase covers the migration path: a database
// created before the owner column must gain it on open, because CREATE TABLE IF
// NOT EXISTS silently skips a table that is already there and the failure would
// otherwise be a scan error at runtime rather than at startup.
func TestOwnerColumnAddedToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// The images table exactly as it was before ownership existed, with a row
	// in it so the migration has to preserve data rather than recreate.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE images (
  ref TEXT PRIMARY KEY,
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO images(ref, data, state, updated_at)
VALUES('legacy:v1', '{"ref":"legacy:v1","state":"READY"}', 'READY', 1);
`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-existing database: %v", err)
	}
	defer st.Close()

	// The pre-existing row survives and reads as unowned, which is what keeps
	// it visible to every caller after the upgrade.
	got, err := st.GetImage("legacy:v1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("migration lost the pre-existing image")
	}
	if got.Owner != "" {
		t.Errorf("owner = %q, want empty for a pre-existing image", got.Owner)
	}

	// The owner column is now usable: an owner-scoped list has to run its
	// WHERE clause against it, which fails outright if the column is missing.
	if err := st.PutImage(img("new:v1", "user-a", ImageBuilt)); err != nil {
		t.Fatalf("write with owner after migration: %v", err)
	}
	scoped, err := st.ListImages("user-a")
	if err != nil {
		t.Fatalf("owner-scoped list after migration: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped images = %d, want 2 (own + legacy unowned)", len(scoped))
	}

	// Opening again must be a no-op rather than an error: the duplicate-column
	// complaint is the expected outcome on an up-to-date database.
	st.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	again.Close()
}
