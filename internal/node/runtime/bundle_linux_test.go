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
// looking at which members the bundle has, so an empty memory path must produce
// a bundle without that member rather than an empty one.
//
// An empty member would be worse than a missing one — restore would try to load
// a zero-length memory image and the guest would fault against nothing.
func TestBundleOmitsMemoryWhenNotIncluded(t *testing.T) {
	src := t.TempDir()
	rootfs := writeFile(t, src, "rootfs.img", "filesystem bytes")

	var buf bytes.Buffer
	if err := writeSnapshotBundle(&buf, "", "", rootfs); err != nil {
		t.Fatalf("write memoryless bundle: %v", err)
	}

	dest := t.TempDir()
	// The rootfs member is staged, not expanded: it is decoded onto the sandbox's
	// device later, while the provider assembles it.
	staged := filepath.Join(dest, "rootfs")
	paths, err := readSnapshotBundle(&buf, dest, staged)
	if err != nil {
		t.Fatalf("read memoryless bundle: %v", err)
	}

	if got, ok := paths[snapshotMemFile]; ok {
		t.Errorf("memoryless bundle contains a memory member at %q", got)
	}
	if got, ok := paths[snapshotStateFile]; ok {
		t.Errorf("memoryless bundle contains a vmstate member at %q", got)
	}
	if paths[snapshotRootfsFile] == "" {
		t.Fatal("memoryless bundle has no rootfs member; nothing would be restored")
	}

	// Decoding the staged member is what a restore does through SeedWritable, so
	// the round trip is checked here: staging that loses the extents would leave
	// the restored guest reading the base image.
	restored := writeFile(t, t.TempDir(), "layer.img", "")
	stage := &snapshotStage{rootfs: staged}
	if err := stage.SeedWritable(restored); err != nil {
		t.Fatalf("seed writable layer: %v", err)
	}
	content, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "filesystem bytes" {
		t.Errorf("restored rootfs = %q, want the original content", content)
	}
}

// TestBundleWithMemoryCarriesAllMembers is the other half: a full checkpoint has
// to keep producing all three, since dropping one would silently turn a resume
// into a reboot.
func TestBundleWithMemoryCarriesAllMembers(t *testing.T) {
	src := t.TempDir()
	state := writeFile(t, src, "vmstate", "device state")
	mem := writeFile(t, src, "memory", "guest memory")
	rootfs := writeFile(t, src, "rootfs.img", "filesystem bytes")

	var buf bytes.Buffer
	if err := writeSnapshotBundle(&buf, state, mem, rootfs); err != nil {
		t.Fatalf("write full bundle: %v", err)
	}

	dest := t.TempDir()
	paths, err := readSnapshotBundle(&buf, dest, filepath.Join(dest, "rootfs"))
	if err != nil {
		t.Fatalf("read full bundle: %v", err)
	}
	for _, name := range []string{snapshotStateFile, snapshotMemFile, snapshotRootfsFile} {
		if paths[name] == "" {
			t.Errorf("full bundle is missing member %q", name)
		}
	}
}

// TestMemorylessBundleIsSmaller records the size difference, which is the
// secondary reason to want this: guest memory dominates a snapshot, so a
// filesystem-only checkpoint of a mostly-idle sandbox is far cheaper to store
// and to ship to another node.
func TestMemorylessBundleIsSmaller(t *testing.T) {
	src := t.TempDir()
	// A memory image is dense — Firecracker writes every byte — so even a
	// modest one dwarfs the extents of a lightly used filesystem.
	mem := filepath.Join(src, "memory")
	if err := os.WriteFile(mem, bytes.Repeat([]byte("m"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	state := writeFile(t, src, "vmstate", "device state")
	rootfs := writeFile(t, src, "rootfs.img", "small")

	var full, memoryless bytes.Buffer
	if err := writeSnapshotBundle(&full, state, mem, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotBundle(&memoryless, "", "", rootfs); err != nil {
		t.Fatal(err)
	}
	if memoryless.Len() >= full.Len() {
		t.Errorf("memoryless bundle is %d bytes, full is %d: dropping memory saved nothing",
			memoryless.Len(), full.Len())
	}
}
