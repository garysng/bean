//go:build linux

package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The node-local index from "which image, on what CPU" to a warm snapshot.
//
// A warm snapshot is one guest that was booted once for an image and checkpointed
// with its memory, so that later creates of that image restore instead of booting.
// The saving is the point: a cold create costs about 5 CPU-seconds of host CPU and
// a restore costs almost none, so throughput is bounded by `cores / 5` until the
// boot is removed rather than made faster (docs/warm-snapshots.md).
//
// Two decisions are encoded here and both are load-bearing.
//
// **The key is the image digest, not its reference.** A tag that has moved names
// different content, and a warm snapshot found by tag would restore an environment
// captured from the image the tag used to name. That failure is silent -- the
// restore succeeds, the guest runs, and only the contents are wrong -- which is why
// the digest is the key even though it costs a lookup. An image with no digest (a
// build's output, a commit, or one converted before digests were recorded) is
// therefore not warmable, and the correct response is to boot.
//
// **The key includes the CPU.** Guest memory records what the CPU it booted on
// offered, and vendor and family cannot be masked away (cpu_template.go), so a
// memory snapshot only restores on a compatible CPU. A heterogeneous fleet needs
// one warm snapshot per CPU generation; that is not a defect of this design but the
// same constraint that already makes the scheduler refuse an incompatible restore.
//
// A miss is ordinary, never an error. A node whose CPU has no warm snapshot boots
// as it did before this existed -- otherwise adding a machine of a new generation
// to a cluster would break creates on it.

// warmSuffix is the extension of a warm snapshot bundle.
const warmSuffix = ".warm"

// warmKey identifies a warm snapshot: one image's content, on one kind of CPU.
type warmKey struct {
	// Digest is the image's manifest digest. Empty means the image cannot be
	// warmed, which is a state the caller has to handle rather than an error.
	Digest string
	// Vendor and Family are the host CPU's identity, as reported by
	// HostCPUIdentity.
	Vendor string
	Family int32
	// Template is the masking policy guests boot under on this node. It belongs in
	// the key because a snapshot taken under a template is portable across models
	// in a way one taken without it is not, so the two are not interchangeable even
	// on the same silicon.
	Template CPUTemplate
}

// warmable reports whether this key can name a warm snapshot.
//
// Only the digest is checked. A node always has a vendor and family, and an empty
// template is a legitimate value meaning "no masking" -- but an image with no
// digest has no identity that survives a tag moving, and keying on anything else
// is the silent failure this whole file is arranged to avoid.
func (k warmKey) warmable() bool { return k.Digest != "" }

// filename is the key's on-disk name.
//
// Hashed rather than composed, because a digest contains a colon and a template
// could contain anything an operator typed, and a key that has to be sanitised is
// a key that can collide after sanitising. The hash is over a separator-delimited
// form so that two different keys cannot produce the same input string -- without
// the separators, {"ab", "c"} and {"a", "bc"} would hash identically.
//
// The digest is kept in the name as well, truncated, so an operator listing the
// directory can tell which image an entry belongs to. It is not what makes the
// name unique; the hash is.
func (k warmKey) filename() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		k.Digest, k.Vendor, fmt.Sprint(k.Family), string(k.Template),
	}, "\x00")))
	short := strings.TrimPrefix(k.Digest, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	return short + "-" + hex.EncodeToString(sum[:8]) + warmSuffix
}

// snapshotID is the identity a restore from this entry uses.
//
// It doubles as the snapshot-cache key, which is what makes the second and every
// later restore of a warm snapshot cheap: the node keeps the unpacked form under
// this id, so only the first restore on a node pays to unpack it. Prefixed so it
// cannot be confused with a control-plane snapshot id in a log or a cache
// directory listing.
func (k warmKey) snapshotID() string {
	return "warm_" + strings.TrimSuffix(k.filename(), warmSuffix)
}

// warmStore holds one node's warm snapshots.
//
// A directory of files, with no index. The filename is derived from the key, so a
// lookup is a stat rather than a read of shared state that could disagree with the
// filesystem -- the same reasoning as the image sidecars in internal/node/image:
// an entry deleted by hand takes its own record with it, and a half-written one
// cannot corrupt the record of any other.
type warmStore struct {
	dir string

	// mu guards reading, which is a counter per bundle rather than a flag: several
	// creates restore from one warm bundle at the same time, which is the fan-out
	// case this whole feature is for, so "in use" has to survive one of them
	// finishing.
	mu      sync.Mutex
	reading map[string]int
}

// newWarmStore returns a store rooted at dir. The directory is created lazily, so
// a node that never warms anything leaves no trace.
func newWarmStore(dir string) *warmStore {
	return &warmStore{dir: dir, reading: map[string]int{}}
}

// hold marks a bundle as being read, and returns the release.
//
// Needed because unlinking a file another process has open succeeds on Linux: the
// restore would finish normally and the bundle would be gone, leaving the node
// quietly cold for that image. That failure reports nothing, which is why eviction
// consults this rather than relying on the filesystem to refuse.
func (s *warmStore) hold(name string) func() {
	s.mu.Lock()
	if s.reading == nil {
		s.reading = map[string]int{}
	}
	s.reading[name]++
	s.mu.Unlock()

	var once bool
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if once {
			return
		}
		once = true
		if s.reading[name] <= 1 {
			delete(s.reading, name)
			return
		}
		s.reading[name]--
	}
}

// inUse reports whether a bundle is being read right now.
func (s *warmStore) inUse(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reading[name] > 0
}

// Path is where a key's bundle lives, whether or not it exists.
func (s *warmStore) Path(k warmKey) string {
	return filepath.Join(s.dir, k.filename())
}

// Lookup returns the bundle path for a key, or false.
//
// A zero-length file is treated as absent. It is what an interrupted warm leaves
// behind, and a restore from it would fail after the sandbox directory and the
// device were already built -- a miss here costs a boot, which is the behaviour
// this feature is an optimisation over.
func (s *warmStore) Lookup(k warmKey) (string, bool) {
	if !k.warmable() {
		return "", false
	}
	path := s.Path(k)
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return "", false
	}
	return path, true
}

// Create returns a file to write a key's bundle into, plus a commit function.
//
// Written to a temporary name and renamed on commit, so a concurrent Lookup never
// finds a partial bundle. That matters more here than for most staged writes: a
// warm snapshot is written once and read by every later create of its image, so a
// truncated one that was visible would make an image slow-and-broken across the
// whole node rather than failing once.
//
// The returned commit is nil-safe to skip: abandoning the file without calling it
// leaves the temporary behind, which Clean removes.
func (s *warmStore) Create(k warmKey) (f *os.File, commit func() error, err error) {
	if !k.warmable() {
		return nil, nil, fmt.Errorf("fc: image has no digest, so it cannot be warmed")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("fc: create warm dir: %w", err)
	}
	final := s.Path(k)
	tmp, err := os.CreateTemp(s.dir, filepath.Base(final)+".tmp.*")
	if err != nil {
		return nil, nil, fmt.Errorf("fc: create warm bundle: %w", err)
	}
	return tmp, func() error {
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("fc: sync warm bundle: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("fc: close warm bundle: %w", err)
		}
		return os.Rename(tmp.Name(), final)
	}, nil
}

// Remove deletes a key's bundle. A missing one is not an error, so a retried
// reclaim is safe.
func (s *warmStore) Remove(k warmKey) error {
	if err := os.Remove(s.Path(k)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Clean removes temporaries left by a warm that did not finish.
//
// Called at startup rather than on a timer: a temporary can only be orphaned by a
// process that died, so the moment a new one starts is exactly when the set of
// orphans is knowable and stable. A running noded's own temporaries must not be
// swept, which a timer would have to reason about and this does not.
func (s *warmStore) Clean() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".tmp.") {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil &&
			!os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// List reports the warm snapshots this node holds, by filename and size.
//
// Filenames rather than keys, because the key cannot be recovered from the name --
// the hash is one-way by design. This is for reporting size and count upward, not
// for lookup; a caller wanting to know whether a specific image is warm asks
// Lookup, which reconstructs the name from the key it already has.
func (s *warmStore) List() (map[string]int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), warmSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = info.Size()
	}
	return out, nil
}
