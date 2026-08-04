package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A node reports which images it holds so the scheduler can prefer a node that
// already has one, and so a prewarm job can show progress.
//
// The reference cannot be recovered from the filename: refToFilename encodes
// separators to keep the name safe as a path, and that mapping is deliberately
// not reversible. So each image is accompanied by a small sidecar naming the
// reference it came from. Storing it beside the image rather than in a single
// index means a half-written conversion cannot corrupt the record of every other
// image, and a manually deleted image takes its own record with it.

const (
	// imageSuffix is the extension of a base image file.
	imageSuffix = ".ext4"
	// refSuffix is the extension of the sidecar naming its reference.
	refSuffix = ".ref"
)

// recordRef notes which reference an image file corresponds to.
func recordRef(imageDir, imageRef, digest string) error {
	name, err := refToFilename(imageRef)
	if err != nil {
		return err
	}
	rec := map[string]string{"ref": imageRef}
	// The digest is what makes an image identifiable independently of the name it
	// was fetched under, and nothing else on the node records it: refToFilename is
	// a string encoding that does not resolve anything, so python:3.12 and
	// python@sha256:... are unrelated cache entries even for the same image.
	//
	// A warm snapshot (GitHub #26) has to be keyed on it rather than on the
	// reference, because a tag that moves must not serve the environment captured
	// from the image it used to name. That failure is silent -- the wrong snapshot
	// restores successfully -- which is why the digest is recorded at the moment it
	// is known instead of being re-resolved later against a registry that may have
	// moved on.
	//
	// Omitted rather than written empty when unknown, so a reader can tell "this
	// image predates the field" from "this image has no digest", and a build, whose
	// output never had a manifest, does not claim one.
	if digest != "" {
		rec["digest"] = digest
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Written atomically: a truncated sidecar would make the image invisible to
	// the scheduler even though it is usable.
	path := filepath.Join(imageDir, name+refSuffix)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CachedImage is what a node knows about one image it holds.
//
// The digest is carried alongside the size because both come from the same
// sidecar read, and separating them would mean scanning the directory twice to
// answer two questions about the same file.
type CachedImage struct {
	// SizeBytes is the apparent size of the prepared image file.
	SizeBytes int64
	// Digest is the manifest digest the reference resolved to, or "" for an image
	// with no manifest -- a build's output, or a commit of a sandbox's filesystem.
	Digest string
	// Warm reports that a warm snapshot for this image exists on this node, so a
	// create placed here restores instead of booting.
	//
	// Not filled in by this package, which knows about image files and not about
	// snapshots. The runtime sets it, because it owns the warm store and is the only
	// thing that can answer for this node's CPU. Kept on this struct anyway so the
	// node reports one view of an image rather than two lists a reader has to join.
	Warm bool
}

// cachedImages lists the images in a directory with their sizes and digests.
//
// An image without a sidecar is skipped rather than guessed at: reporting a
// wrong reference would send the scheduler's affinity scoring after an image
// that is not there.
func cachedImages(imageDir string) (map[string]CachedImage, error) {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]CachedImage{}, nil
		}
		return nil, err
	}

	out := map[string]CachedImage{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), refSuffix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(imageDir, e.Name()))
		if err != nil {
			continue
		}
		var rec struct {
			Ref string `json:"ref"`
			// Read here rather than through cachedDigest, which would re-open the
			// same file this loop has already read, once per image.
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Ref == "" {
			continue
		}

		base := strings.TrimSuffix(e.Name(), refSuffix)
		info, err := os.Stat(filepath.Join(imageDir, base+imageSuffix))
		if err != nil {
			// The sidecar outlived its image; nothing is cached.
			continue
		}
		out[rec.Ref] = CachedImage{SizeBytes: info.Size(), Digest: rec.Digest}
	}
	return out, nil
}

// cachedDigest reports the manifest digest an image reference resolved to when
// it was converted, or "" if that is not recorded.
//
// Read from the sidecar rather than from a registry, deliberately. Re-resolving
// the tag would ask what it points at now, while the question is what the local
// file was built from -- and those differ precisely when it matters, after a tag
// has moved. A lookup keyed on the answer from the registry would then match a
// warm snapshot captured from an image this node no longer has.
//
// An empty return is not an error. Images converted before this was recorded have
// no digest, and the caller's correct response is to treat the lookup as a miss
// and boot, which is the same thing it does on a node whose CPU has no warm
// snapshot.
func cachedDigest(imageDir, imageRef string) (string, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(imageDir, name+refSuffix))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var rec struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		// A corrupt sidecar is reported: it means the image is present but its
		// identity is unknown, and silently treating that as "no digest" would hide
		// a damaged cache behind a slow path that still works.
		return "", fmt.Errorf("image: read digest for %s: %w", imageRef, err)
	}
	return rec.Digest, nil
}

// cachedRefs caches the image listing so a heartbeat, which fires every few
// seconds, does not scan the directory each time.
//
// Staleness is detected from the directory's modification time rather than by
// explicit invalidation. Publishing an image writes a sidecar into this
// directory, which updates its mtime — so every path that adds an image
// invalidates the cache by construction. The earlier design had each writer call
// an invalidate method, and the build path was added without one: a built image
// stayed invisible to the scheduler until the node restarted.
type cachedRefs struct {
	mu    sync.Mutex
	stamp time.Time
	refs  map[string]CachedImage
}

func (c *cachedRefs) get(imageDir string) (map[string]CachedImage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if info, err := os.Stat(imageDir); err == nil {
		// The cached listing stands as long as the directory has not changed
		// since it was taken.
		if c.refs != nil && !info.ModTime().After(c.stamp) {
			return copyRefs(c.refs), nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("image: stat image dir: %w", err)
	}

	// The stamp is taken before the scan, not from the directory's mtime after
	// it. A write landing during the scan leaves an mtime the scan may not have
	// seen; comparing against a pre-scan time means the next call re-reads
	// rather than trusting a listing that could be missing that image.
	stamp := time.Now()
	refs, err := cachedImages(imageDir)
	if err != nil {
		return nil, fmt.Errorf("image: list cached images: %w", err)
	}
	c.refs = refs
	c.stamp = stamp
	return copyRefs(refs), nil
}

// copyRefs returns a copy, so a caller cannot mutate what later reports carry.
func copyRefs(refs map[string]CachedImage) map[string]CachedImage {
	out := make(map[string]CachedImage, len(refs))
	for k, v := range refs {
		out[k] = v
	}
	return out
}

// createSparse makes a file of the given size without allocating it, which is
// what keeps a provisioned-but-unused image or copy-on-write store from costing
// its full size on disk. The exclusive create is deliberate: two concurrent
// conversions must not end up writing the same file.
func createSparse(path string, sizeMiB int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("image: create %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	if err := f.Truncate(sizeMiB << 20); err != nil {
		return fmt.Errorf("image: size %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Digest implements Provider for a provider whose images live in imageDir.
//
// A free function rather than a method because all three providers hold the
// directory under a different field name and would otherwise each carry the same
// two-line body.
func digestOf(imageDir, imageRef string) (string, error) {
	return cachedDigest(imageDir, imageRef)
}
