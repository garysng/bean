//go:build linux

package reclaim

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestParseLoopLineSeparatesDeletedMarker covers the signal the whole loop-device
// half of this rests on. The kernel appends " (deleted)" to the backing path once
// the file is unlinked, and treating that as part of the path would put the device
// outside BaseDir and make it invisible to reconciliation.
func TestParseLoopLineSeparatesDeletedMarker(t *testing.T) {
	for _, tc := range []struct {
		name, back string
		wantPath   string
		wantDel    bool
	}{
		{"/dev/loop15", "/var/lib/bean/sandboxes/sbx_a/cow.img (deleted)",
			"/var/lib/bean/sandboxes/sbx_a/cow.img", true},
		{"/dev/loop3", "/var/lib/bean/images/py.ext4",
			"/var/lib/bean/images/py.ext4", false},
		// A path that legitimately ends in the word without the marker's space.
		{"/dev/loop4", "/var/lib/bean/images/(deleted)",
			"/var/lib/bean/images/(deleted)", false},
	} {
		got := parseLoopLine(tc.name, tc.back)
		if got.BackingFile != tc.wantPath || got.Deleted != tc.wantDel {
			t.Errorf("parseLoopLine(%q) = %q,%v want %q,%v",
				tc.back, got.BackingFile, got.Deleted, tc.wantPath, tc.wantDel)
		}
	}
}

// TestRemoveDMRefusesForeignNames checks the guard behind the caller's filter. It
// is redundant today, which is the point: the cost of a future caller reaching
// this without filtering is another workload's storage.
func TestRemoveDMRefusesForeignNames(t *testing.T) {
	h := &LinuxHost{BaseDir: t.TempDir()}
	for _, name := range []string{"docker-253:1-pool", "nexus-bean-x", "", "bean-"} {
		if err := h.RemoveDM(name); err == nil {
			t.Errorf("RemoveDM(%q) was accepted", name)
		}
	}
}

func TestDetachLoopRefusesNonLoopDevices(t *testing.T) {
	h := &LinuxHost{BaseDir: t.TempDir()}
	for _, dev := range []string{"/dev/sda1", "/dev/mapper/bean-x", "", "loop0"} {
		if err := h.DetachLoop(dev); err == nil {
			t.Errorf("DetachLoop(%q) was accepted", dev)
		}
	}
}

// TestRemoveSandboxDirStaysUnderBaseDir is the guard on the only recursive delete
// in this package. The name reaching it comes from a directory listing, but a
// traversal in that value would take the delete anywhere on the host.
func TestRemoveSandboxDirStaysUnderBaseDir(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	h := &LinuxHost{BaseDir: filepath.Join(base, "sandboxes")}
	if err := os.MkdirAll(filepath.Join(h.BaseDir, "sbx_a"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../outside", "..", ".", "", "a/b", "/etc"} {
		if err := h.RemoveSandboxDir(name); err == nil {
			t.Errorf("RemoveSandboxDir(%q) was accepted", name)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a traversal escaped BaseDir: %v", err)
	}

	if err := h.RemoveSandboxDir("sbx_a"); err != nil {
		t.Fatalf("removing a real sandbox directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.BaseDir, "sbx_a")); !os.IsNotExist(err) {
		t.Errorf("sandbox directory survived removal: %v", err)
	}
}

// TestListSandboxDirsToleratesMissingBaseDir: a node that has never created a
// sandbox has nothing to reconcile, and turning that into an error would make
// every fresh node log a reconciliation failure at startup.
func TestListSandboxDirsToleratesMissingBaseDir(t *testing.T) {
	h := &LinuxHost{BaseDir: filepath.Join(t.TempDir(), "absent")}
	dirs, err := h.ListSandboxDirs()
	if err != nil {
		t.Fatalf("missing base directory reported as an error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("dirs = %v, want none", dirs)
	}
}

// TestListSandboxDirsSkipsFiles keeps stray files out of the candidate set: only a
// directory can be a sandbox, and a file named like one would otherwise be handed
// to RemoveSandboxDir.
func TestListSandboxDirsSkipsFiles(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sbx_a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "stray.img"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &LinuxHost{BaseDir: base}
	dirs, err := h.ListSandboxDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "sbx_a" {
		t.Errorf("dirs = %v, want [sbx_a]", dirs)
	}
}

// TestDMAlreadyGoneIsNotAFailure pins the distinction a 300-sandbox burst exposed.
//
// dmsetup exits 1 for every error, so the exit code cannot tell "already gone" from
// "still busy" -- and the caller does opposite things with them. A failed removal is
// read as "something still holds this open", which marks the mapping alive and then
// blocks reclaiming the loop device and directory behind it. Measured: 109 mappings
// reported as unreclaimable with "No such device or address", each leaking the two
// resources behind it, on a node the control plane had just declared LOST.
//
// The busy cases must keep failing, because forcing a removal there would pull the
// device out from under a guest that is still writing to it.
func TestDMAlreadyGoneIsNotAFailure(t *testing.T) {
	gone := []string{
		// The exact wording measured on the 128-core host.
		"dmsetup: device-mapper: remove ioctl on bean-sbx_x failed: No such device or address\nCommand failed.",
		"dmsetup: device-mapper: remove ioctl on bean-sbx_y failed: No such device",
		"dmsetup: device does not exist",
	}
	for _, msg := range gone {
		if !dmAlreadyGone(errors.New(msg)) {
			t.Errorf("treated an already-removed mapping as a failure: %q\n"+
				"The caller then marks it alive and leaks the loop device and "+
				"directory behind it", msg)
		}
	}

	stillReal := []string{
		"dmsetup: device-mapper: remove ioctl on bean-sbx_z failed: Device or resource busy",
		"dmsetup: device-mapper: remove ioctl failed: Operation not permitted",
		"exec: \"dmsetup\": executable file not found in $PATH",
		// Not a device error at all; must not be swallowed.
		"dmsetup: invalid argument",
	}
	for _, msg := range stillReal {
		if dmAlreadyGone(errors.New(msg)) {
			t.Errorf("swallowed a real failure as already-gone: %q\n"+
				"A busy device that is silently reported as reclaimed would let the "+
				"next pass delete the layer under a running guest", msg)
		}
	}
}
