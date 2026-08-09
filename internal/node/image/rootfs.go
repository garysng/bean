// Package image assembles a sandbox rootfs as a block device on the node.
//
// The microVM tier needs a block device, not a directory: Firecracker attaches
// virtio-blk drives, and the guest mounts them. This package owns the step
// from "image reference" to "device path the VM can boot", which is the only
// part of the fc tier that depends on how images are stored.
//
// Keeping it behind an interface means the runtime does not know whether the
// device came from overlaybd with an S3 backing store or from a plain file. It
// also means the runtime is testable without a kernel module loaded.
package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotCached reports an image that has no local device and could not be
// materialised, so the caller can surface a distinct failure rather than a
// generic create error.
var ErrNotCached = errors.New("image: not cached on this node")

// Rootfs is a mountable representation of an image plus its writable layer.
type Rootfs struct {
	// Device is the path the VM attaches, e.g. /dev/ublkb0 or a backing file.
	Device string
	// ReadOnly reports whether writes are possible. A sandbox always gets a
	// writable rootfs; a shared base layer does not.
	ReadOnly bool
	// Writable is the path holding this sandbox's changes, if the provider
	// keeps them separately from Device. Checkpointing captures it.
	Writable string
	// release tears down whatever the provider set up.
	release func() error
}

// Release frees the device. It is safe to call more than once, so cleanup on
// the failure path does not have to track whether it already ran.
func (r *Rootfs) Release() error {
	if r == nil || r.release == nil {
		return nil
	}
	release := r.release
	r.release = nil
	return release()
}

// PrepareOptions carries what a provider needs beyond the image reference.
type PrepareOptions struct {
	// SizeMiB bounds the writable layer; zero means the provider's default.
	SizeMiB int64

	// FSManifestDigest, when set, resolves the read-only lowers from a snapshot's
	// sealed filesystem chain instead of from imageRef. The chain already includes
	// the base image's layers -- a snapshot's manifest is the base layers plus the
	// one sealed on capture -- so this is resolved as a digest reference through
	// the same store path an image tag uses, and a fresh empty writable goes on
	// top. Empty means resolve from imageRef, the cold-start path.
	//
	// It is the restore counterpart of sealing on capture: the filesystem travels
	// as this identity rather than as bytes, so no extents are replayed and the
	// snapshot layer is shared with the base image's in the store rather than
	// copied. Overlaybd-tier only -- the local tier restores through its own tar
	// checkpoint, never through a provider.
	FSManifestDigest string
}

// Provider turns an image reference into a rootfs. Implementations differ in
// how the bytes arrive — lazily from object storage, or copied up front — but
// not in what the runtime receives.
type Provider interface {
	// Name identifies the provider in logs and node capabilities.
	Name() string
	// Prepare returns a writable rootfs for one sandbox.
	Prepare(ctx context.Context, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error)
	// Prewarm materialises an image's read-only layers without creating a
	// sandbox, so a later Prepare is fast. It is the node side of the
	// control plane's prewarm job.
	Prewarm(ctx context.Context, imageRef string) error
	// Cached reports the images held locally, with the size and digest of each.
	// The node reports this to the control plane, which is what lets the scheduler
	// prefer a node that already has an image, lets a prewarm job show progress,
	// and lets a warm snapshot be found by the image's digest rather than by a tag
	// that may since have moved.
	Cached() (map[string]CachedImage, error)
	// Config reports the OCI configuration recorded for an image, or nil if the
	// image has none.
	//
	// On the Provider rather than read straight from disk by the runtime because
	// only the provider knows where it keeps its images, which is the same reason
	// Digest is here.
	//
	// Nil is a normal answer rather than an error: an image converted before configs
	// were recorded has none, and neither does a build's output. A caller then starts
	// the sandbox from its request alone, which is what every image did before this
	// was stored.
	Config(imageRef string) (*Config, error)
	// Digest reports the manifest digest a reference resolved to when it was
	// prepared on this node, or "" if that is not recorded.
	//
	// Read from what the node has, never re-resolved against a registry. The
	// question is what the local file was built from, and that differs from what
	// the tag points at now precisely when it matters. An empty return is not an
	// error: the image simply cannot be warmed, and booting is the correct answer.
	Digest(imageRef string) (string, error)
}

// FileProvider backs each sandbox with a sparse file formatted as ext4. It
// exists so the fc tier is exercisable — and the runtime testable — on hosts
// without overlaybd or its kernel module, and as the fallback when a node's
// object-store path is unavailable.
//
// It copies the base image up front rather than fetching lazily, so it is not
// what production should use for large images.
type FileProvider struct {
	// BaseDir holds per-sandbox rootfs files.
	BaseDir string
	// ImageDir holds prepared base images, named by their sanitised ref.
	ImageDir string
	// DefaultSizeMiB applies when a spec does not bound the disk.
	DefaultSizeMiB int64

	cache cachedRefs
}

func (p *FileProvider) Name() string { return "file" }

func (p *FileProvider) Prepare(ctx context.Context, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error) {
	if sandboxID == "" {
		return nil, errors.New("image: sandbox id required")
	}
	sizeMiB := opts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = p.DefaultSizeMiB
	}
	if sizeMiB <= 0 {
		sizeMiB = 2048
	}

	dir := filepath.Join(p.BaseDir, sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("image: create sandbox dir: %w", err)
	}
	path := filepath.Join(dir, "rootfs.ext4")

	base, err := p.basePath(imageRef)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := cloneSparse(base, path, sizeMiB); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	return &Rootfs{
		Device:   path,
		Writable: path,
		release:  func() error { return os.RemoveAll(dir) },
	}, nil
}

// Prewarm makes an image's base file available locally.
func (p *FileProvider) Prewarm(ctx context.Context, imageRef string) error {
	_, err := p.basePath(imageRef)
	return err
}

// Cached lists the base images present locally.
func (p *FileProvider) Cached() (map[string]CachedImage, error) {
	return p.cache.get(p.ImageDir)
}

// basePath resolves the prepared base image for a ref, reporting ErrNotCached
// when it is absent. Converting an OCI image into a base file is the image
// service's job; this provider only consumes the result.
func (p *FileProvider) basePath(imageRef string) (string, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.ImageDir, name+imageSuffix)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotCached, imageRef)
		}
		return "", err
	}
	return path, nil
}

// Digest reports what this node recorded for the image.
func (p *FileProvider) Digest(imageRef string) (string, error) {
	return digestOf(p.ImageDir, imageRef)
}

// Config reports the image configuration this node recorded.
func (p *FileProvider) Config(imageRef string) (*Config, error) {
	return cachedConfig(p.ImageDir, imageRef)
}
