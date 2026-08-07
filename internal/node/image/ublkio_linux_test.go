//go:build linux

package image

import (
	"bytes"
	"crypto/rand"
	"os"
	"sync"
	"testing"
)

// memBackend is a ublkBackend over a byte slice, so the protocol can be tested without
// overlaybd, a registry, or a converted image in the way.
//
// Locked because the queue may handle several slots concurrently; a data race here would
// present as corrupted guest data, which is the hardest kind of bug to attribute.
type memBackend struct {
	mu      sync.Mutex
	data    []byte
	flushes int
}

func (b *memBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if off >= int64(len(b.data)) {
		return 0, nil
	}
	n := copy(p, b.data[off:])
	return n, nil
}

func (b *memBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if off+int64(len(p)) > int64(len(b.data)) {
		return 0, os.ErrInvalid
	}
	return copy(b.data[off:], p), nil
}

func (b *memBackend) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushes++
	return nil
}

// TestUblkServesRealIOOnHardware is the test that decides whether the ublk path works.
//
// Everything before this established that commands are accepted and a device node
// appears. None of that proves data moves: a device can exist, report the right size, and
// return zeros or hang on its first read. This writes a pattern through the block device,
// reads it back through a fresh handle to defeat the page cache, and checks the bytes.
//
// The backend is memory rather than overlaybd on purpose. What is unproven here is the
// ublk protocol -- the descriptor mapping, the USER_COPY offset encoding, the
// commit-and-fetch loop -- and putting LSMT decoding behind it would mean a failure could
// be either one.
func TestUblkServesRealIOOnHardware(t *testing.T) {
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("no ublk on this host (%v)", err)
	}

	const size = 8 << 20
	backend := &memBackend{data: make([]byte, size)}

	ctrl, err := openUblkControl()
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer ctrl.Close()

	dev, err := attachUblk(ctrl, backend, size)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Registered immediately: a leaked ublk device holds a minor and a kernel thread on
	// a host running other people's workloads.
	defer func() {
		if err := dev.detach(); err != nil {
			t.Errorf("detach left something behind: %v", err)
		}
	}()

	t.Logf("device %s (dev_id=%d, %d bytes)", dev.Device, dev.DevID, dev.Size)

	// A pattern rather than zeros, so a device that simply returns zeroed pages cannot
	// pass. Random rather than a repeating byte, so a read that returns the wrong offset
	// fails too.
	pattern := make([]byte, 64<<10)
	if _, err := rand.Read(pattern); err != nil {
		t.Fatal(err)
	}
	const writeOff = 1 << 20

	// Buffered, then synced, then re-read through a fresh handle with the cache dropped.
	// O_DIRECT would be stricter but needs a page-aligned buffer, which Go does not
	// provide without manual alignment; dropping the cache achieves the same end.
	w, err := os.OpenFile(dev.Device, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev.Device, err)
	}
	if _, err := w.WriteAt(pattern, writeOff); err != nil {
		w.Close()
		t.Fatalf("write to %s: %v", dev.Device, err)
	}
	if err := w.Sync(); err != nil {
		w.Close()
		t.Fatalf("sync %s: %v", dev.Device, err)
	}
	w.Close()

	// Checked at the backend first: if the bytes are not here, the write never reached
	// userspace and the read below would be testing the page cache.
	backend.mu.Lock()
	landed := bytes.Equal(backend.data[writeOff:writeOff+len(pattern)], pattern)
	flushes := backend.flushes
	backend.mu.Unlock()
	if !landed {
		t.Fatalf("the pattern did not reach the backend, so the write path is wrong: "+
			"the queue read the request but the data did not arrive (flushes=%d)", flushes)
	}
	t.Logf("write path works: %d bytes reached the backend, flushes=%d",
		len(pattern), flushes)

	// A fresh handle, and the cache dropped, so the read goes to the device rather than
	// to pages the write left behind. This is the mistake the snapshot work already made
	// once: an assertion that passes against a corrupt device because it never left
	// memory.
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o644); err != nil {
		t.Logf("could not drop caches (%v); the read below may be served from cache", err)
	}
	r, err := os.Open(dev.Device)
	if err != nil {
		t.Fatalf("reopen %s: %v", dev.Device, err)
	}
	defer r.Close()

	got := make([]byte, len(pattern))
	if _, err := r.ReadAt(got, writeOff); err != nil {
		t.Fatalf("read from %s: %v", dev.Device, err)
	}
	if !bytes.Equal(got, pattern) {
		firstDiff := 0
		for firstDiff < len(got) && got[firstDiff] == pattern[firstDiff] {
			firstDiff++
		}
		t.Fatalf("read back different bytes: first difference at %d of %d. The device "+
			"exists and answers, so this is the offset encoding or the read path rather "+
			"than the protocol", firstDiff, len(got))
	}
	t.Logf("read path works: %d bytes round-tripped through %s", len(got), dev.Device)
}
