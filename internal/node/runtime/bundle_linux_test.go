//go:build linux

package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates a file with known content and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBundleOmitsMemoryWhenNotIncluded is what makes a filesystem-only
// checkpoint work: restore decides whether to load a snapshot or boot a guest by
// looking at whether the bundle has a memory member, so an empty memory path must
// produce a bundle without that member rather than an empty one.
//
// An empty member would be worse than a missing one -- restore would try to load
// a zero-length memory image and the guest would fault against nothing. The
// filesystem is no longer in the bundle at all; it is resolved from the
// snapshot's sealed layer chain, so a memoryless bundle is genuinely empty.
func TestBundleOmitsMemoryWhenNotIncluded(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSnapshotBundle(&buf, "", "", false); err != nil {
		t.Fatalf("write memoryless bundle: %v", err)
	}

	dest := t.TempDir()
	paths, err := readSnapshotBundle(&buf, dest)
	if err != nil {
		t.Fatalf("read memoryless bundle: %v", err)
	}

	if got, ok := paths[snapshotMemFile]; ok {
		t.Errorf("memoryless bundle contains a memory member at %q", got)
	}
	if got, ok := paths[snapshotStateFile]; ok {
		t.Errorf("memoryless bundle contains a vmstate member at %q", got)
	}
}

// TestBundleWithMemoryCarriesMemoryAndState is the other half: a full checkpoint
// has to keep producing both the machine state and the memory image, since
// dropping one would silently turn a resume into a reboot.
func TestBundleWithMemoryCarriesMemoryAndState(t *testing.T) {
	src := t.TempDir()
	state := writeFile(t, src, "vmstate", "device state")
	mem := writeFile(t, src, "memory", "guest memory")

	var buf bytes.Buffer
	if err := writeSnapshotBundle(&buf, state, mem, false); err != nil {
		t.Fatalf("write full bundle: %v", err)
	}

	dest := t.TempDir()
	paths, err := readSnapshotBundle(&buf, dest)
	if err != nil {
		t.Fatalf("read full bundle: %v", err)
	}
	for _, name := range []string{snapshotStateFile, snapshotMemFile} {
		if paths[name] == "" {
			t.Errorf("full bundle is missing member %q", name)
		}
	}

	// The round trip must preserve content: a corrupt vmstate or memory image
	// would fail the load rather than boot, so the bytes are checked.
	if got, err := os.ReadFile(paths[snapshotMemFile]); err != nil {
		t.Fatal(err)
	} else if string(got) != "guest memory" {
		t.Errorf("restored memory = %q, want the original content", got)
	}
}

// TestMemorylessBundleIsSmaller records the size difference, which is the
// secondary reason to want this: guest memory dominates a snapshot, so a
// filesystem-only checkpoint is far cheaper to store and to ship. With the
// filesystem out of the bundle entirely, a memoryless bundle carries no members
// at all.
func TestMemorylessBundleIsSmaller(t *testing.T) {
	src := t.TempDir()
	// A memory image is dense -- Firecracker writes every byte -- so even a
	// modest one dwarfs an empty bundle.
	mem := filepath.Join(src, "memory")
	if err := os.WriteFile(mem, bytes.Repeat([]byte("m"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	state := writeFile(t, src, "vmstate", "device state")

	var full, memoryless bytes.Buffer
	if err := writeSnapshotBundle(&full, state, mem, false); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotBundle(&memoryless, "", "", false); err != nil {
		t.Fatal(err)
	}
	if memoryless.Len() >= full.Len() {
		t.Errorf("memoryless bundle is %d bytes, full is %d: dropping memory saved nothing",
			memoryless.Len(), full.Len())
	}
}
