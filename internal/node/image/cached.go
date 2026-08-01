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
func recordRef(imageDir, imageRef string) error {
	name, err := refToFilename(imageRef)
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"ref": imageRef})
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

// cachedImages lists the images in a directory with their apparent sizes.
//
// An image without a sidecar is skipped rather than guessed at: reporting a
// wrong reference would send the scheduler's affinity scoring after an image
// that is not there.
func cachedImages(imageDir string) (map[string]int64, error) {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int64{}, nil
		}
		return nil, err
	}

	out := map[string]int64{}
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
		out[rec.Ref] = info.Size()
	}
	return out, nil
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
	refs  map[string]int64
}

func (c *cachedRefs) get(imageDir string) (map[string]int64, error) {
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

// copyRefs returns a copy, so a caller cannot mutate what later heartbeats
// report.
func copyRefs(refs map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(refs))
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
