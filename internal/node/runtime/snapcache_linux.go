//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Unpacking a snapshot bundle is the whole remaining cost of a restore: the
// memory image is written out in full before the VM starts, which measured
// ~1060ms for a 512 MiB guest. Every restore of a given snapshot unpacks to the
// same bytes, so the result is kept and reused.
//
// The guest never writes to the memory image — Firecracker maps it
// copy-on-write, verified by checksumming the file after a guest wrote to its
// own memory — so one unpacked copy safely serves any number of restores.
//
// The writable rootfs is deliberately not cached. It is per-sandbox by
// definition: two sandboxes restored from one snapshot diverge as soon as either
// writes, so each needs its own copy on its own device.

// snapCache holds unpacked snapshot state, keyed by snapshot id.
type snapCache struct {
	dir string

	mu      sync.Mutex
	pending map[string]*snapUnpack
}

// snapUnpack is one in-flight unpack. Callers that arrive while it runs read its
// result instead of starting their own.
type snapUnpack struct {
	done  chan struct{}
	entry snapEntry
	err   error
}

func newSnapCache(dir string) *snapCache {
	return &snapCache{dir: dir, pending: map[string]*snapUnpack{}}
}

// snapEntry is an unpacked bundle's reusable parts.
type snapEntry struct {
	StatePath string
	MemPath   string
}

// Lookup returns the cached entry for a snapshot, or false if it is absent.
func (c *snapCache) Lookup(id string) (snapEntry, bool) {
	if id == "" {
		return snapEntry{}, false
	}
	e := c.entryFor(id)
	// Both members must be present: a half-written entry would load into a VM
	// that then faults against nothing.
	for _, p := range []string{e.StatePath, e.MemPath} {
		st, err := os.Stat(p)
		if err != nil || st.Size() == 0 {
			return snapEntry{}, false
		}
	}
	return e, true
}

// Fill produces the cache entry for a snapshot, calling unpack to build it.
//
// Concurrent restores of the same snapshot are common — a batch fans out from one
// checkpoint — so only the first builds the shared entry and the rest wait for it
// rather than each writing the same bytes over the top of the others.
//
// unpack is nevertheless called by every caller, with an empty dir when another
// is already building the entry. That is not a redundant call: each restore has
// its own stream to drain and its own writable layer to stage, neither of which
// is shareable. A waiter that skipped unpack would leave its sender blocked on a
// stream nobody reads, and would give its sandbox the base image in place of the
// snapshot's filesystem — silently, since the guest would boot and simply not
// have the files.
func (c *snapCache) Fill(id string, unpack func(dir string) (snapEntry, error)) (snapEntry, error) {
	if id == "" {
		return snapEntry{}, errors.New("fc: snapshot cache needs an id")
	}

	for {
		if e, ok := c.Lookup(id); ok {
			// The shared entry exists, but this caller's own stream and writable
			// layer still have to be dealt with.
			if _, err := unpack(""); err != nil {
				return snapEntry{}, err
			}
			return e, nil
		}

		c.mu.Lock()
		if inflight, ok := c.pending[id]; ok {
			c.mu.Unlock()
			// This caller's per-restore work happens before the wait, not after:
			// the sender is streaming into a pipe, and blocking on another
			// restore's build while refusing to read would deadlock as soon as
			// that pipe filled.
			if _, err := unpack(""); err != nil {
				return snapEntry{}, err
			}
			// Then wait for the shared entry. Reading the outcome rather than
			// re-checking the cache is what keeps a second build from starting:
			// the entry becomes visible on disk slightly after the first caller
			// finishes, and a waiter that raced that window would otherwise
			// decide nothing was cached and build the same bytes again.
			<-inflight.done
			if inflight.err == nil {
				return inflight.entry, nil
			}
			// The shared build failed. This caller cannot retry it: its own stream
			// is already consumed, so there is nothing left to read.
			return snapEntry{}, inflight.err
		}
		u := &snapUnpack{done: make(chan struct{})}
		c.pending[id] = u
		c.mu.Unlock()

		u.entry, u.err = c.unpackInto(id, unpack)

		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		close(u.done)

		return u.entry, u.err
	}
}

// unpackInto builds an entry in a temporary directory and publishes it under the
// snapshot's id only once it is complete, so a failed or interrupted build cannot
// leave a partial entry that later restores would trust.
func (c *snapCache) unpackInto(id string, unpack func(dir string) (snapEntry, error)) (snapEntry, error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return snapEntry{}, fmt.Errorf("fc: create snapshot cache: %w", err)
	}
	tmp, err := os.MkdirTemp(c.dir, ".unpack-*")
	if err != nil {
		return snapEntry{}, fmt.Errorf("fc: snapshot cache temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	entry, err := unpack(tmp)
	if err != nil {
		return snapEntry{}, err
	}
	// A filesystem-only checkpoint has neither member, and that is not a defect:
	// there is nothing to cache, because the cache exists to avoid rebuilding
	// guest memory. An empty entry tells the caller to boot rather than load.
	if entry.StatePath == "" && entry.MemPath == "" {
		return snapEntry{}, nil
	}
	// One without the other is a defect: a load against a missing memory image
	// leaves the guest faulting on nothing.
	if entry.StatePath == "" || entry.MemPath == "" {
		return snapEntry{}, errors.New("fc: snapshot bundle has vmstate or memory but not both")
	}

	final := filepath.Join(c.dir, id)
	if err := os.Rename(tmp, final); err != nil {
		// Another restore of the same snapshot may have published first, which is
		// a success: the contents are identical by construction. This caller's own
		// stream is already consumed, so nothing more is owed to it.
		if e, ok := c.Lookup(id); ok {
			return e, nil
		}
		return snapEntry{}, fmt.Errorf("fc: publish snapshot cache entry: %w", err)
	}
	return c.entryFor(id), nil
}

func (c *snapCache) entryFor(id string) snapEntry {
	dir := filepath.Join(c.dir, id)
	return snapEntry{
		StatePath: filepath.Join(dir, snapshotStateFile),
		MemPath:   filepath.Join(dir, snapshotMemFile),
	}
}
