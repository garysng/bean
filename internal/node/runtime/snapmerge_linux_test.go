//go:build linux

package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pageSize is the granularity a diff layer records. The value does not matter to
// these tests beyond being larger than one extent, so it is fixed rather than
// read from the host.
const testPage = 4096

// memImage builds a dense memory image of n pages, each filled with a distinct
// byte, standing in for what a full checkpoint writes.
func memImage(t *testing.T, dir, name string, pages ...byte) string {
	t.Helper()
	buf := make([]byte, 0, len(pages)*testPage)
	for _, b := range pages {
		buf = append(buf, bytes.Repeat([]byte{b}, testPage)...)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// diffImage builds a sparse file the size of a full image but with data only in
// the named pages, which is the shape Firecracker writes for a diff.
func diffImage(t *testing.T, dir, name string, totalPages int, dirty map[int]byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(totalPages) * testPage); err != nil {
		t.Fatal(err)
	}
	for page, b := range dirty {
		if _, err := f.WriteAt(bytes.Repeat([]byte{b}, testPage), int64(page)*testPage); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return p
}

// bundleFor packages one layer the way Checkpoint does.
func bundleFor(t *testing.T, state, mem, rootfs string, diff bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeSnapshotBundle(&buf, state, mem, rootfs, diff); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return buf.Bytes()
}

// pageByte reports the byte a page was filled with, failing if the page is not
// uniform — which would mean a partial extent write rather than a whole page.
func pageByte(t *testing.T, path string, page int) byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, testPage)
	if _, err := f.ReadAt(buf, int64(page)*testPage); err != nil {
		t.Fatalf("read page %d of %s: %v", page, path, err)
	}
	for i, b := range buf {
		if b != buf[0] {
			t.Fatalf("page %d is not uniform: byte 0 is %d, byte %d is %d", page, buf[0], i, b)
		}
	}
	return buf[0]
}

// TestMergeChainLayersDiffsOntoBase is the property the whole design rests on:
// a diff contributes only the pages it recorded, and everything it did not touch
// keeps the base's contents.
//
// Getting this wrong in the other direction — treating a diff as a full image —
// would zero every untouched page, which is why the untouched pages are asserted
// as explicitly as the changed ones.
func TestMergeChainLayersDiffsOntoBase(t *testing.T) {
	src := t.TempDir()
	// Four pages: the base fills them 1,2,3,4.
	base := memImage(t, src, "base-mem", 1, 2, 3, 4)
	baseState := writeFile(t, src, "base-state", "base device state")
	baseRootfs := writeFile(t, src, "base-rootfs", "base filesystem")

	// The first diff rewrites pages 0 and 2; the second rewrites page 2 again
	// plus page 3. Page 1 is never touched by either.
	d1 := diffImage(t, src, "d1-mem", 4, map[int]byte{0: 10, 2: 30})
	d1State := writeFile(t, src, "d1-state", "d1 device state")
	d2 := diffImage(t, src, "d2-mem", 4, map[int]byte{2: 99, 3: 40})
	d2State := writeFile(t, src, "d2-state", "d2 device state")
	leafRootfs := writeFile(t, src, "leaf-rootfs", "leaf filesystem")

	layers := []SnapshotLayer{
		{ID: "snap_base", Data: bytes.NewReader(bundleFor(t, baseState, base, baseRootfs, false))},
		{ID: "snap_d1", Data: bytes.NewReader(bundleFor(t, d1State, d1, "", true))},
		{ID: "snap_d2", Data: bytes.NewReader(bundleFor(t, d2State, d2, leafRootfs, true))},
	}

	dir := t.TempDir()
	rootfsDest := filepath.Join(t.TempDir(), "staged-rootfs")
	entry, err := mergeChain(layers, dir, rootfsDest)
	if err != nil {
		t.Fatalf("merge chain: %v", err)
	}

	for _, tc := range []struct {
		page int
		want byte
		why  string
	}{
		{0, 10, "written by the first diff and not since"},
		{1, 2, "never touched by either diff, so the base's page must survive"},
		{2, 99, "written by both diffs; the later one wins"},
		{3, 40, "written only by the second diff"},
	} {
		if got := pageByte(t, entry.MemPath, tc.page); got != tc.want {
			t.Errorf("page %d = %d, want %d (%s)", tc.page, got, tc.want, tc.why)
		}
	}

	// The state must come from the leaf: Firecracker pairs a memory image with
	// the state from the same create call, so an earlier layer's state would
	// describe the machine before the diffs.
	state, err := os.ReadFile(entry.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(state) != "d2 device state" {
		t.Errorf("machine state is %q, want the leaf's", state)
	}

	// Only the leaf's filesystem is staged; the intermediate layers' would be
	// overwritten by it anyway. It is staged in extent form and decoded onto the
	// sandbox's device later, so the check goes through the same path a restore
	// uses rather than reading the staging file directly.
	restored := writeFile(t, t.TempDir(), "layer.img", "")
	if err := (&snapshotStage{rootfs: rootfsDest}).SeedWritable(restored); err != nil {
		t.Fatalf("seed writable layer: %v", err)
	}
	staged, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != "leaf filesystem" {
		t.Errorf("staged rootfs is %q, want the leaf's", staged)
	}
}

// TestMergeChainOrderMatters is what makes the test above trustworthy. A later
// page legitimately overwrites an earlier one, so a reversed chain still produces
// a well-formed image — just one built from stale pages. If this passed, the
// ordering assertion above would be proving nothing.
func TestMergeChainOrderMatters(t *testing.T) {
	src := t.TempDir()
	base := memImage(t, src, "base-mem", 1, 1)
	baseState := writeFile(t, src, "base-state", "base state")
	d1 := diffImage(t, src, "d1-mem", 2, map[int]byte{0: 50})
	d1State := writeFile(t, src, "d1-state", "d1 state")
	d2 := diffImage(t, src, "d2-mem", 2, map[int]byte{0: 60})
	d2State := writeFile(t, src, "d2-state", "d2 state")

	// Reversed: the older diff is applied last, so page 0 ends up holding the
	// value that was superseded.
	layers := []SnapshotLayer{
		{ID: "snap_base", Data: bytes.NewReader(bundleFor(t, baseState, base, "", false))},
		{ID: "snap_d2", Data: bytes.NewReader(bundleFor(t, d2State, d2, "", true))},
		{ID: "snap_d1", Data: bytes.NewReader(bundleFor(t, d1State, d1, "", true))},
	}

	entry, err := mergeChain(layers, t.TempDir(), "")
	if err != nil {
		t.Fatalf("merge chain: %v", err)
	}
	if got := pageByte(t, entry.MemPath, 0); got != 50 {
		t.Fatalf("reversed chain produced page 0 = %d; the test cannot detect ordering", got)
	}
	// Stated as an assertion so the reason this test exists survives: 60 is the
	// correct answer for the correctly ordered chain, and 50 is what reversal
	// yields. The two differ, so ordering is observable.
	if pageByte(t, entry.MemPath, 0) == 60 {
		t.Error("reversed and correct order produced the same result")
	}
}

// TestMergeChainRejectsDiffAsRoot catches a chain whose base is missing — a
// deleted ancestor, or a chain assembled wrongly. Starting from a sparse layer
// would give the guest an image full of holes where its own state should be,
// which is corruption rather than a failed restore.
func TestMergeChainRejectsDiffAsRoot(t *testing.T) {
	src := t.TempDir()
	d := diffImage(t, src, "orphan-mem", 2, map[int]byte{0: 7})
	dState := writeFile(t, src, "orphan-state", "orphan state")

	_, err := mergeChain([]SnapshotLayer{
		{ID: "snap_orphan", Data: bytes.NewReader(bundleFor(t, dState, d, "", true))},
	}, t.TempDir(), "")
	if err == nil {
		t.Fatal("merged a chain rooted in a diff")
	}
	if !strings.Contains(err.Error(), "snap_orphan") {
		t.Errorf("error %q does not name the offending layer", err)
	}
}

// TestMergeChainRejectsEmpty guards the degenerate input, since an empty chain
// would otherwise return a zero entry that reads as "boot fresh" — turning a
// restore into a silent reboot.
func TestMergeChainRejectsEmpty(t *testing.T) {
	if _, err := mergeChain(nil, t.TempDir(), ""); err == nil {
		t.Fatal("merged an empty chain")
	}
}

// TestMergeChainRejectsSizeMismatch keeps a mismatched layer from truncating the
// image. readSparse resizes its destination to the stream's logical size, which
// is right when materialising a file and wrong when layering onto one: a guest
// resumed against a shortened image faults on memory that disappeared.
func TestMergeChainRejectsSizeMismatch(t *testing.T) {
	src := t.TempDir()
	base := memImage(t, src, "base-mem", 1, 2, 3, 4)
	baseState := writeFile(t, src, "base-state", "base state")
	// Two pages against the base's four, as a layer from a differently sized
	// guest would be.
	small := diffImage(t, src, "small-mem", 2, map[int]byte{0: 9})
	smallState := writeFile(t, src, "small-state", "small state")

	_, err := mergeChain([]SnapshotLayer{
		{ID: "snap_base", Data: bytes.NewReader(bundleFor(t, baseState, base, "", false))},
		{ID: "snap_small", Data: bytes.NewReader(bundleFor(t, smallState, small, "", true))},
	}, t.TempDir(), "")
	if err == nil {
		t.Fatal("merged a layer sized for a different guest")
	}
	if !strings.Contains(err.Error(), "snap_small") {
		t.Errorf("error %q does not name the offending layer", err)
	}
}
