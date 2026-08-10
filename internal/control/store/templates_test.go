package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func tpl(name, owner string, source TemplateSource) *Template {
	return &Template{
		ID: NewID(PrefixTemplate), Name: name, State: TemplateReady,
		Source: source, Owner: owner, CreatedAt: time.Now(),
	}
}

func TestListTemplatesUnfilteredReturnsEverything(t *testing.T) {
	st := openTestStore(t)
	for _, in := range []*Template{
		tpl("python:3.12", "", TemplateConverted),
		tpl("team-a/app:v1", "user-a", TemplateBuilt),
		tpl("team-b/app:v1", "user-b", TemplateBuilt),
	} {
		if err := st.PutTemplate(in); err != nil {
			t.Fatal(err)
		}
	}
	// An empty owner is the operator's view, and the behaviour of every
	// deployment that configures no identity.
	got, err := st.ListTemplates("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("templates = %d, want 3", len(got))
	}
}

func TestListTemplatesScopesToOwnerPlusUnowned(t *testing.T) {
	st := openTestStore(t)
	for _, in := range []*Template{
		tpl("python:3.12", "", TemplateConverted),
		tpl("team-a/app:v1", "user-a", TemplateBuilt),
		tpl("team-b/app:v1", "user-b", TemplateBuilt),
	} {
		if err := st.PutTemplate(in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListTemplates("user-a")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, i := range got {
		names[i.Name] = true
	}
	if !names["team-a/app:v1"] {
		t.Error("owner cannot see their own template")
	}
	// Unowned means shared, not hidden: an upgraded deployment must still show
	// the base templates it can run.
	if !names["python:3.12"] {
		t.Error("unowned template hidden from owner")
	}
	if names["team-b/app:v1"] {
		t.Error("another owner's template leaked into the listing")
	}
}

func TestPutTemplatePreservesOwnerAcrossReload(t *testing.T) {
	st := openTestStore(t)
	in := tpl("mine:v1", "user-a", TemplateBuilt)
	if err := st.PutTemplate(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTemplate(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "user-a" {
		t.Errorf("owner = %q, want user-a", got.Owner)
	}
	if got.Source != TemplateBuilt {
		t.Errorf("source = %q, want built", got.Source)
	}
}

// TestGetTemplateByNameTakesMostRecent covers name resolution: a name is not
// unique (a rebuild reuses it), so resolution takes the most recently updated.
func TestGetTemplateByNameTakesMostRecent(t *testing.T) {
	st := openTestStore(t)
	older := tpl("app:v1", "user-a", TemplateBuilt)
	older.UpdatedAt = time.Unix(1, 0)
	if err := st.PutTemplate(older); err != nil {
		t.Fatal(err)
	}
	newer := tpl("app:v1", "user-a", TemplateBuilt)
	newer.UpdatedAt = time.Unix(2, 0)
	if err := st.PutTemplate(newer); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTemplateByName("app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != newer.ID {
		t.Fatalf("by-name resolved to %v, want most recent %s", got, newer.ID)
	}
}

// TestTemplatesOwnerColumnAddedToExistingDatabase covers the migration path: a
// database created before the owner column must gain it on open, because CREATE
// TABLE IF NOT EXISTS silently skips a table that is already there and the
// failure would otherwise be a scan error at runtime rather than at startup.
func TestTemplatesOwnerColumnAddedToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// The templates table exactly as it was before ownership existed, with a row
	// in it so the migration has to preserve data rather than recreate.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE templates (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL,
  state TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO templates(id, name, data, state, updated_at)
VALUES('tpl_legacy', 'legacy:v1', '{"id":"tpl_legacy","name":"legacy:v1","state":"READY"}', 'READY', 1);
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
	got, err := st.GetTemplate("tpl_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("migration lost the pre-existing template")
	}
	if got.Owner != "" {
		t.Errorf("owner = %q, want empty for a pre-existing template", got.Owner)
	}

	// The owner column is now usable: an owner-scoped list has to run its
	// WHERE clause against it, which fails outright if the column is missing.
	if err := st.PutTemplate(tpl("new:v1", "user-a", TemplateBuilt)); err != nil {
		t.Fatalf("write with owner after migration: %v", err)
	}
	scoped, err := st.ListTemplates("user-a")
	if err != nil {
		t.Fatalf("owner-scoped list after migration: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped templates = %d, want 2 (own + legacy unowned)", len(scoped))
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
