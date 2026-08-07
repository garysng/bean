//go:build linux

package image

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestUblkCowDeviceOnHardware is the end-to-end claim: copy-on-write over ublk, no
// losetup and no dmsetup.
//
// This is what the whole ublk path is for. The dm-snapshot provider spends 3.8 s of a
// 4.5 s create forking losetup twice and dmsetup once per sandbox, at ~26 ms a call; a
// ublk device over the same two files needs none of them. The unit tests above prove the
// copy-on-write arithmetic and the IO test proves the protocol -- this checks they compose
// into a block device a guest could boot from.
//
// The reads go through a reopened handle after dropping the cache, for the reason
// decisions.md 3.0 records: a read served from the page cache passes against a device
// that is returning the wrong bytes.
func TestUblkCowDeviceOnHardware(t *testing.T) {
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("no ublk on this host (%v)", err)
	}

	const size = 16 << 20
	dir := t.TempDir()

	// A base with recognisable content, so "came from the base" and "came from a hole"
	// are different observable outcomes.
	basePath := filepath.Join(dir, "base.img")
	base := make([]byte, size)
	for i := range base {
		base[i] = byte('A' + i%26)
	}
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		t.Fatal(err)
	}

	backend, err := newFileBackend(basePath, filepath.Join(dir, "overlay.img"), size)
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer backend.Close()

	ctrl, err := openUblkControl()
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	defer ctrl.Close()

	dev, err := attachUblk(ctrl, backend, size)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() {
		if err := dev.detach(); err != nil {
			t.Errorf("detach: %v", err)
		}
	}()
	t.Logf("device %s over base+overlay, no losetup or dmsetup", dev.Device)

	// The base must be visible through the device before anything is written. A device
	// that returns zeros here would still pass a write-then-read test, which is why this
	// is checked first.
	r, err := os.Open(dev.Device)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	head := make([]byte, 4096)
	if _, err := r.ReadAt(head, 0); err != nil {
		r.Close()
		t.Fatalf("read the base through the device: %v", err)
	}
	r.Close()
	if !bytes.Equal(head, base[:len(head)]) {
		t.Fatalf("the device does not show the base image; first byte %q, want %q",
			head[0], base[0])
	}
	t.Logf("base is visible through the device")

	// Now write, and check the base is untouched afterwards -- that is the
	// copy-on-write property, and the file on disk is where it is observable.
	pattern := make([]byte, 8192)
	if _, err := rand.Read(pattern); err != nil {
		t.Fatal(err)
	}
	const writeOff = 1 << 20

	w, err := os.OpenFile(dev.Device, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	if _, err := w.WriteAt(pattern, writeOff); err != nil {
		w.Close()
		t.Fatalf("write: %v", err)
	}
	if err := w.Sync(); err != nil {
		w.Close()
		t.Fatalf("sync: %v", err)
	}
	w.Close()

	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o644); err != nil {
		t.Logf("could not drop caches (%v); the read below may be cached", err)
	}

	r2, err := os.Open(dev.Device)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()

	got := make([]byte, len(pattern))
	if _, err := r2.ReadAt(got, writeOff); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		first := 0
		for first < len(got) && got[first] == pattern[first] {
			first++
		}
		t.Fatalf("written bytes did not survive the round trip; first difference at %d",
			first)
	}
	t.Logf("write round-tripped through the device")

	// The base file must be byte-identical: the write went to the overlay, which is the
	// whole point of sharing one base across sandboxes.
	after, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, base) {
		t.Error("the base image was modified; it is shared between sandboxes, so a write " +
			"reaching it would corrupt every other sandbox using the same image")
	} else {
		t.Logf("base image untouched: copy-on-write holds")
	}

	// And the overlay is sparse -- the property that makes a sandbox cost kilobytes
	// rather than its nominal size.
	if st, err := os.Stat(filepath.Join(dir, "overlay.img")); err == nil {
		var stat = st.Sys()
		t.Logf("overlay: apparent %d bytes, sys=%T", st.Size(), stat)
	}
}
