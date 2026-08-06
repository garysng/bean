package image

import (
	"encoding/json"
	"errors"
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
// not reversible. So each image gets a small metadata file naming the reference it
// came from. One file per image rather than a single index means a half-written
// conversion cannot corrupt the record of every other image, and a manually
// deleted image takes its own record with it.
//
// These were called sidecars while the flattening path was the only one, and that
// name has been dropped: it implied a main artefact to sit beside. On the ext4 path
// there was one -- `<ref>.ext4`, with `<ref>.ref` next to it. Under overlaybd there
// is not: layers are named by digest and shared between images, so nothing here
// belongs to one reference. Reading the name literally is what led listCached to
// stat a `<ref>.ext4` that overlaybd never creates, which made every overlaybd
// image report as uncached.

const (
	// imageSuffix is the extension of a base image file, on the backends that
	// flatten an image into one.
	imageSuffix = ".ext4"
	// refSuffix is the extension of the per-image metadata file.
	refSuffix = ".ref"
)

// ImageRecord is what a node knows about one image it holds, as written to disk.
//
// A struct rather than a growing parameter list: this started as (ref, digest) and
// reached five positional arguments, at which point a caller passing the size where the
// digest belonged would still compile.
type ImageRecord struct {
	// Ref is the reference the image was fetched under. Required -- a record without
	// it names nothing, and the listing skips it.
	Ref string `json:"ref"`
	// Digest is the manifest digest the reference resolved to, or "" for an image with
	// no manifest: a build's output, or a commit of a sandbox's filesystem.
	Digest string `json:"digest,omitempty"`
	// Config is the image's OCI configuration, or nil for an image that has none.
	Config *Config `json:"config,omitempty"`
	// SizeBytes is what the image costs on this node, for backends whose image is not
	// a single file the listing can stat. Zero means "stat the flattened file", which
	// is what the ext4 path does.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Layers is the resolved layer chain, base first, for backends that assemble one.
	//
	// Recorded so a repeat create does not have to re-resolve the manifest. Without
	// it every overlaybd create fetches the manifest from the registry -- which the
	// flattening path does not, since its Prepare only looks for a local file. That
	// difference is an availability one, not a latency one: with the registry
	// unreachable, dm-snapshot starts a cached image and overlaybd does not.
	//
	// Empty for the flattening backends, which have no chain to record.
	Layers []RecordedLayer `json:"layers,omitempty"`
}

// RecordedLayer is one layer of a resolved chain, as much of it as is worth keeping.
//
// Deliberately not obdLayer: that type carries local paths and cache directories,
// which are this node's current arrangement rather than facts about the image. Storing
// them would make the record wrong as soon as a layer were reclaimed. What is kept is
// what the registry said, so the chain can be rebuilt against whatever the node holds
// at the time.
type RecordedLayer struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType,omitempty"`
}

// recordRef notes what a node knows about one image.
//
// Everything goes into one file rather than several, so a single atomic write
// publishes it all and a reader cannot catch two parts disagreeing. The config in
// particular has to be here because conversion flattens layers into a filesystem and
// drops the config blob: without it the guest never learns the image's ENV,
// ENTRYPOINT, CMD or WORKDIR.
//
// Absent fields are omitted rather than written as null or zero, so a reader can tell
// "this image predates the field" from "this image genuinely has none" -- which is why
// a build, whose output never had a manifest, does not claim a digest.
func recordRef(imageDir string, rec ImageRecord) error {
	if rec.Ref == "" {
		return errors.New("image: record needs a reference")
	}
	name, err := refToFilename(rec.Ref)
	if err != nil {
		return err
	}
	// The digest is what makes an image identifiable independently of the name it was
	// fetched under, and nothing else on the node records it: refToFilename is a
	// string encoding that does not resolve anything, so python:3.12 and
	// python@sha256:... are unrelated cache entries even for the same image.
	//
	// A warm snapshot (GitHub #26) has to be keyed on it rather than on the reference,
	// because a tag that moves must not serve the environment captured from the image
	// it used to name. That failure is silent -- the wrong snapshot restores
	// successfully -- which is why the digest is recorded at the moment it is known
	// instead of being re-resolved later against a registry that may have moved on.
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Written atomically: a truncated metadata file would make the image invisible to
	// the scheduler even though it is usable.
	//
	// The temporary name is unique per call rather than derived from the final one.
	// Concurrent creates of the same image do reach here together -- layer
	// conversion is deduplicated, recording is not -- and with a shared temp name
	// one writer's rename pulls the file out from under the other, which then fails
	// with ENOENT on a path it had just written. The records are identical, so the
	// fix is to let each writer own its temp file and let the renames overwrite.
	path := filepath.Join(imageDir, name+refSuffix)
	tmp, err := os.CreateTemp(imageDir, name+refSuffix+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CachedImage is what a node knows about one image it holds.
//
// The digest is carried alongside the size because both come from the same
// metadata read, and separating them would mean scanning the directory twice to
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
// An image without a metadata file is skipped rather than guessed at: reporting a
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
			// Set by backends whose image is not a single file next to this record.
			SizeBytes int64 `json:"sizeBytes"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.Ref == "" {
			continue
		}

		if rec.SizeBytes > 0 {
			// An overlaybd image: layer files shared with other images, so there is
			// nothing here to stat and the writer recorded the size instead.
			out[rec.Ref] = CachedImage{SizeBytes: rec.SizeBytes, Digest: rec.Digest}
			continue
		}

		base := strings.TrimSuffix(e.Name(), refSuffix)
		info, err := os.Stat(filepath.Join(imageDir, base+imageSuffix))
		if err != nil {
			// The record outlived its image; nothing is cached.
			continue
		}
		out[rec.Ref] = CachedImage{SizeBytes: info.Size(), Digest: rec.Digest}
	}
	return out, nil
}

// cachedDigest reports the manifest digest an image reference resolved to when
// it was converted, or "" if that is not recorded.
//
// Read from the metadata file rather than from a registry, deliberately. Re-resolving
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
		// A corrupt metadata file is reported: it means the image is present but its
		// identity is unknown, and silently treating that as "no digest" would hide
		// a damaged cache behind a slow path that still works.
		return "", fmt.Errorf("image: read digest for %s: %w", imageRef, err)
	}
	return rec.Digest, nil
}

// cachedConfig reports the OCI configuration recorded for an image, or nil if the
// image has none.
//
// Nil is not an error and callers must handle it: images converted before configs
// were recorded have no entry, and a build's output legitimately has none. The
// correct response is to start the sandbox from the caller's request alone, which
// is what every image did before this was stored -- so an absent config degrades to
// the previous behaviour rather than failing the create.
func cachedConfig(imageDir, imageRef string) (*Config, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(imageDir, name+refSuffix))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec struct {
		Config *Config `json:"config"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("image: read config for %s: %w", imageRef, err)
	}
	return rec.Config, nil
}

// cachedRecord reads everything the node recorded about an image, or nil if it has no
// record.
//
// Whole-record rather than one field at a time, because a caller that wants the layer
// chain wants the digest with it: the chain is only meaningful as "what this digest
// resolved to", and reading them separately would let a reader pair a chain with a
// digest from a later write.
func cachedRecord(imageDir, imageRef string) (*ImageRecord, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(imageDir, name+refSuffix))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec ImageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("image: read record for %s: %w", imageRef, err)
	}
	if rec.Ref == "" {
		// A record naming no reference is not usable, and is reported rather than
		// returned empty: it means the file is present but damaged, which a caller
		// treating nil as "not cached" would silently convert into a re-pull.
		return nil, fmt.Errorf("image: record for %s names no reference", imageRef)
	}
	return &rec, nil
}

// cachedRefs caches the image listing so a heartbeat, which fires every few
// seconds, does not scan the directory each time.
//
// Staleness is detected from the directory's modification time rather than by
// explicit invalidation. Publishing an image writes a metadata file into this
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
