//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The failures this store can have are mostly silent: a key collision serves the
// wrong environment, a partial bundle serves a broken one, and both look like a
// successful restore. So the tests below are about identity and atomicity rather
// than about the file operations, which would fail loudly on their own.

const (
	digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestC = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	digestD = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

func testKey() warmKey {
	return warmKey{Digest: digestA, Vendor: "AuthenticAMD", Family: 23,
		Template: CPUTemplateNone}
}

// TestWarmKeySeparatesEveryField is the core correctness property. Each field is
// varied on its own, and any two keys producing one filename would mean a create
// restoring an environment captured somewhere else -- a different image, or a CPU
// this guest's memory does not describe.
func TestWarmKeySeparatesEveryField(t *testing.T) {
	base := testKey()
	variants := map[string]warmKey{
		"base":           base,
		"other digest":   {Digest: digestB, Vendor: "AuthenticAMD", Family: 23, Template: CPUTemplateNone},
		"other vendor":   {Digest: digestA, Vendor: "GenuineIntel", Family: 23, Template: CPUTemplateNone},
		"other family":   {Digest: digestA, Vendor: "AuthenticAMD", Family: 25, Template: CPUTemplateNone},
		"other template": {Digest: digestA, Vendor: "AuthenticAMD", Family: 23, Template: CPUTemplatePortable},
		"no template":    {Digest: digestA, Vendor: "AuthenticAMD", Family: 23},
	}

	seen := map[string]string{}
	for name, k := range variants {
		got := k.filename()
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q produce the same filename %s: a create would restore "+
				"an environment captured under different conditions", name, prev, got)
		}
		seen[got] = name
	}
}

// TestWarmKeyCannotCollideAcrossFieldBoundaries covers the specific mistake of
// concatenating the fields without separators, where {"ab","c"} and {"a","bc"}
// hash alike.
//
// The fields varied are the *last* two, not the first. An earlier version of this
// test moved the boundary between Digest and Vendor and passed against a
// deliberately broken implementation, because the filename also embeds a truncated
// digest -- so the names differed there even though the hashes were identical, and
// the test was measuring the prefix rather than the hash it claimed to check. The
// digest is held equal here so the hash is the only thing that can distinguish
// them.
func TestWarmKeyCannotCollideAcrossFieldBoundaries(t *testing.T) {
	a := warmKey{Digest: digestA, Vendor: "vendor1", Family: 1, Template: "x"}
	b := warmKey{Digest: digestA, Vendor: "vendor", Family: 11, Template: "x"}
	if a.filename() == b.filename() {
		t.Errorf("keys differing only in where a field boundary falls collide: %s\n"+
			"the fields are being concatenated without a separator, so a warm "+
			"snapshot can be served for the wrong CPU", a.filename())
	}
}

// TestWarmKeyIsStable guards against the name depending on anything but the key.
// A name that varied per process would make every restart lose the node's warm
// snapshots while leaving the files on disk.
func TestWarmKeyIsStable(t *testing.T) {
	k := testKey()
	if k.filename() != testKey().filename() {
		t.Error("the same key produced two filenames")
	}
	if k.snapshotID() != testKey().snapshotID() {
		t.Error("the same key produced two snapshot ids")
	}
}

// TestWarmKeyWithoutADigestIsNotWarmable is the compatibility path. A build's
// output and a commit have no manifest, and an image converted before digests were
// recorded has none either; all three must miss rather than match on "".
func TestWarmKeyWithoutADigestIsNotWarmable(t *testing.T) {
	k := warmKey{Vendor: "AuthenticAMD", Family: 23}
	if k.warmable() {
		t.Error("a key with no digest reports as warmable")
	}
	s := newWarmStore(t.TempDir())
	if _, ok := s.Lookup(k); ok {
		t.Error("Lookup matched a key with no digest")
	}
	if _, _, err := s.Create(k); err == nil {
		t.Error("Create accepted a key with no digest")
	}
}

// TestWarmStoreRoundTrip is the ordinary path: write, commit, find.
func TestWarmStoreRoundTrip(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	k := testKey()

	if _, ok := s.Lookup(k); ok {
		t.Fatal("found a bundle before anything was written")
	}

	f, commit, err := s.Create(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("bundle bytes"); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	path, ok := s.Lookup(k)
	if !ok {
		t.Fatal("committed bundle not found")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bundle bytes" {
		t.Errorf("bundle contents = %q", data)
	}
}

// TestWarmStoreHidesAnUncommittedBundle is the atomicity property. A create
// concurrent with a warm must not find the half-written file: a restore from it
// fails after the sandbox directory and the device are already built, and the
// bundle would stay broken for every later create of that image.
func TestWarmStoreHidesAnUncommittedBundle(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	k := testKey()

	f, _, err := s.Create(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("half a bundle"); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}

	if path, ok := s.Lookup(k); ok {
		t.Errorf("Lookup found an uncommitted bundle at %s", path)
	}
	f.Close()
}

// TestWarmStoreTreatsAnEmptyBundleAsAbsent covers what an interrupted warm leaves
// if the rename somehow ran: a zero-length file. Serving it would turn a create
// into a restore that cannot work, where a miss would merely have booted.
func TestWarmStoreTreatsAnEmptyBundleAsAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "warm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := newWarmStore(dir)
	k := testKey()
	if err := os.WriteFile(s.Path(k), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(k); ok {
		t.Error("Lookup matched a zero-length bundle")
	}
}

// TestWarmStoreCleanRemovesOnlyTemporaries checks the startup sweep does not take
// committed bundles with it, which would silently un-warm the node.
func TestWarmStoreCleanRemovesOnlyTemporaries(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	k := testKey()

	f, commit, err := s.Create(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("keep me"); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	// An abandoned temporary, as a killed noded leaves.
	orphan, _, err := s.Create(warmKey{Digest: digestB, Vendor: "AuthenticAMD", Family: 23})
	if err != nil {
		t.Fatal(err)
	}
	orphanPath := orphan.Name()
	orphan.Close()

	if err := s.Clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("Clean left the temporary %s", orphanPath)
	}
	if _, ok := s.Lookup(k); !ok {
		t.Error("Clean removed a committed bundle")
	}
}

// TestWarmStoreCleanOnAnAbsentDirectory covers a node that has never warmed
// anything. Startup must not fail over it.
func TestWarmStoreCleanOnAnAbsentDirectory(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "never-created"))
	if err := s.Clean(); err != nil {
		t.Errorf("Clean on a missing directory: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Errorf("List on a missing directory: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List reported %v", got)
	}
}

// TestWarmStoreListSkipsTemporaries keeps a partial bundle out of the size a node
// reports, so an interrupted warm cannot look like held capacity.
func TestWarmStoreListSkipsTemporaries(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	f, commit, err := s.Create(testKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("committed"); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}

	tmp, _, err := s.Create(warmKey{Digest: digestB, Vendor: "V", Family: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("List = %v, want only the committed bundle", got)
	}
	for name := range got {
		if strings.Contains(name, ".tmp.") {
			t.Errorf("List reported a temporary: %s", name)
		}
	}
}

// TestWarmSnapshotIDIsDistinguishable keeps a warm id from being mistaken for a
// control-plane snapshot id, which share the node's snapshot cache namespace.
func TestWarmSnapshotIDIsDistinguishable(t *testing.T) {
	id := testKey().snapshotID()
	if !strings.HasPrefix(id, "warm_") {
		t.Errorf("snapshot id %q does not mark itself as warm", id)
	}
	if strings.HasSuffix(id, warmSuffix) {
		t.Errorf("snapshot id %q carries the file extension; it is a cache key, "+
			"not a filename", id)
	}
}
