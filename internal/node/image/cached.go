package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

const refSuffix = ".ref"

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
		info, err := os.Stat(filepath.Join(imageDir, base+".ext4"))
		if err != nil {
			// The sidecar outlived its image; nothing is cached.
			continue
		}
		out[rec.Ref] = info.Size()
	}
	return out, nil
}

// cachedRefs is a small in-process cache so a heartbeat does not scan the image
// directory every few seconds. It is invalidated on conversion rather than
// expiring, since the node is the only thing that adds images.
type cachedRefs struct {
	mu    sync.Mutex
	valid bool
	refs  map[string]int64
}

func (c *cachedRefs) get(imageDir string) (map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid {
		// A copy, so a caller cannot mutate the cache.
		out := make(map[string]int64, len(c.refs))
		for k, v := range c.refs {
			out[k] = v
		}
		return out, nil
	}
	refs, err := cachedImages(imageDir)
	if err != nil {
		return nil, fmt.Errorf("image: list cached images: %w", err)
	}
	c.refs = refs
	c.valid = true
	out := make(map[string]int64, len(refs))
	for k, v := range refs {
		out[k] = v
	}
	return out, nil
}

func (c *cachedRefs) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// invalidateCache lets a conversion tell a provider its image list changed.
func (p *FileProvider) invalidateCache() { p.cache.invalidate() }
