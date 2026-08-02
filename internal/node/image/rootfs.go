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

	// SeedWritable, when set, populates the writable layer from a checkpoint. It
	// is called with the layer's path once it exists at its final size and
	// before the device is assembled from it.
	//
	// The ordering is the whole reason this is a provider concern rather than
	// something the runtime does after Prepare returns. A device-mapper
	// snapshot reads its exception table into kernel memory when the device is
	// activated and never re-reads it, so bytes written to the copy-on-write
	// store afterwards are invisible: the device keeps serving the base image.
	// That failure is silent — the guest's own metadata still describes the
	// files, so they appear with the right size and read back as zeroes.
	SeedWritable func(dest string) error
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
	// Cached reports the images held locally and their sizes. The node reports
	// this in its heartbeat, which is what lets the scheduler prefer a node
	// that already has an image and lets a prewarm job show progress.
	Cached() (map[string]int64, error)
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

	// This provider has no device to assemble, so seeding is just a write. It is
	// still done here rather than left to the caller so that every provider
	// establishes the writable layer at the same point in the sequence.
	if opts.SeedWritable != nil {
		if err := opts.SeedWritable(path); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("image: seed writable layer: %w", err)
		}
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
func (p *FileProvider) Cached() (map[string]int64, error) {
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
