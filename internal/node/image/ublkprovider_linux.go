//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// UblkProvider serves each sandbox's rootfs from a ublk block device.
//
// It is the same copy-on-write arrangement as DevMapperProvider -- one shared read-only
// base per image, a sparse overlay per sandbox -- reached through a different transport.
// dm-snapshot reaches it by forking losetup twice and dmsetup once per sandbox; this
// reaches it by writing io_uring commands. The layout on disk is deliberately identical,
// so the image conversion, the cache and the prewarm path are shared rather than
// reimplemented.
//
// Measured motivation, on a 128-core host at 256 concurrent creates: fc_rootfs was 3.8 s
// of a 4.5 s create on dm-snapshot, and each losetup/dmsetup call costs ~26 ms whatever
// the concurrency. Teardown was worse and did not improve with the kernel: 4.0 s for 128
// devices on both 5.15 and 6.8, because overlaybd's tcmu daemon serialises through one
// netlink socket that upstream warns against using concurrently.
//
// Requires kernel 6.0 or later. A node without ublk gets DevMapperProvider, which is why
// Available exists and why it names the kernel version rather than only failing.
type UblkProvider struct {
	// BaseDir holds per-sandbox overlays, one directory each.
	BaseDir string
	// ImageDir holds the shared base images. The same directory DevMapperProvider uses
	// and the same filenames, so an image converted for one backend serves the other.
	ImageDir string
	// DefaultSizeMiB bounds a sandbox's device when its spec does not.
	DefaultSizeMiB int64

	// ctrl is the shared handle on /dev/ublk-control. One per node: the control device
	// is a singleton, and a handle plus a ring per sandbox would be an fd and a mapping
	// each for commands that happen twice in a sandbox's life.
	ctrlOnce sync.Once
	ctrl     *ublkControl
	ctrlErr  error

	// cache memoises the on-disk image listing, shared in shape with the other
	// providers so Cached costs the same whichever backend a node runs.
	cache cachedRefs

	mu sync.Mutex
	// attached tracks live devices so teardown can find them and a leak is attributable
	// to a sandbox rather than only to a device id.
	attached map[string]*ublkDevice
}

func NewUblkProvider(baseDir, imageDir string, defaultSizeMiB int64) *UblkProvider {
	return &UblkProvider{
		BaseDir:        baseDir,
		ImageDir:       imageDir,
		DefaultSizeMiB: defaultSizeMiB,
		attached:       map[string]*ublkDevice{},
	}
}

func (p *UblkProvider) Name() string { return "ublk" }

// Available reports whether this host can run the provider, so a node refuses placements
// it cannot honour rather than failing every create.
func (p *UblkProvider) Available() error {
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		return fmt.Errorf("image: ublk unavailable: /dev/ublk-control is absent, which "+
			"means either a kernel older than 6.0 or ublk_drv not loaded "+
			"(modprobe ublk_drv): %w", err)
	}
	c, err := p.control()
	if err != nil {
		return err
	}
	features, err := c.Features()
	if err != nil {
		return fmt.Errorf("image: ublk control device present but unusable: %w", err)
	}
	// Checked at startup rather than at the first create: the alternative to USER_COPY
	// is mapping the driver's buffers, which this server does not do, and without the
	// check the failure would be a device whose reads return garbage.
	if features&ublkFUserCopy == 0 {
		return fmt.Errorf("image: this kernel's ublk lacks UBLK_F_USER_COPY "+
			"(features=%#x), which this implementation requires", features)
	}
	return nil
}

func (p *UblkProvider) control() (*ublkControl, error) {
	p.ctrlOnce.Do(func() {
		p.ctrl, p.ctrlErr = openUblkControl()
	})
	return p.ctrl, p.ctrlErr
}

// Prepare assembles one sandbox's rootfs as a ublk device.
func (p *UblkProvider) Prepare(ctx context.Context, sandboxID, imageRef string, opts PrepareOptions) (rootfs *Rootfs, err error) {
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

	basePath, err := p.basePath(imageRef)
	if err != nil {
		return nil, err
	}

	ctrl, err := p.control()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(p.BaseDir, sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("image: create sandbox dir: %w", err)
	}

	var cleanup []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	cleanup = append(cleanup, func() { os.RemoveAll(dir) })

	// The device must be at least as large as the base, for the reason the overlaybd
	// path had to learn the hard way: a device smaller than the filesystem on it is
	// refused by the guest kernel with a geometry error the caller never sees.
	size := sizeMiB << 20
	if st, serr := os.Stat(basePath); serr == nil && st.Size() > size {
		size = st.Size()
	}

	overlayPath := filepath.Join(dir, "overlay.img")
	backend, err := newFileBackend(basePath, overlayPath, size)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = backend.Close() })

	// A restore's contents go in before the device is assembled. The same ordering
	// dm-snapshot needs (decisions.md 3.0): bytes written after the device exists are
	// not in the state it read, and on a full snapshot that failure is silent.
	if opts.SeedWritable != nil {
		if err := opts.SeedWritable(overlayPath); err != nil {
			return nil, fmt.Errorf("image: seed writable layer: %w", err)
		}
	}

	dev, err := attachUblk(ctrl, backend, size)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = dev.detach() })

	p.mu.Lock()
	p.attached[sandboxID] = dev
	p.mu.Unlock()

	// Linked under a local name because Firecracker resolves drive paths relative to
	// its working directory.
	link := filepath.Join(dir, "rootfs.img")
	if err := os.Symlink(dev.Device, link); err != nil {
		return nil, fmt.Errorf("image: link rootfs device: %w", err)
	}

	return &Rootfs{
		Device: link,
		// The overlay is what a checkpoint captures: it holds everything this sandbox
		// changed, and the base is reproducible from the image.
		Writable: overlayPath,
		release: func() error {
			p.mu.Lock()
			delete(p.attached, sandboxID)
			p.mu.Unlock()

			var errs []error
			// Ordered device-then-backend: the device is still serving reads from the
			// backend until it is detached, and closing the files first would fail
			// every in-flight request instead of draining them.
			if err := dev.detach(); err != nil {
				errs = append(errs, err)
			}
			if err := backend.Close(); err != nil {
				errs = append(errs, err)
			}
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove sandbox dir: %w", err))
			}
			return errors.Join(errs...)
		},
	}, nil
}

// basePath is where an image's converted base lives.
//
// Shared with DevMapperProvider by construction: same directory, same filename encoding.
// So an image prewarmed for either backend serves both, and switching a node between them
// does not reconvert anything.
func (p *UblkProvider) basePath(imageRef string) (string, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.ImageDir, name+".ext4")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("image: base for %s is not on this node: %w", imageRef, err)
	}
	return path, nil
}

// Prewarm makes sure the image's base is on this node.
//
// Identical to DevMapperProvider's, because the base is the same file in the same place.
// A node that switches backends does not reconvert anything, and a prewarm done for one
// serves the other -- which is the reason the layout was kept identical rather than
// designed afresh.
func (p *UblkProvider) Prewarm(ctx context.Context, imageRef string) error {
	_, err := p.basePath(imageRef)
	return err
}

// Cached lists the base images present locally.
func (p *UblkProvider) Cached() (map[string]CachedImage, error) {
	return p.cache.get(p.ImageDir)
}

// Digest reports the image's digest, which is what a warm snapshot is keyed on.
func (p *UblkProvider) Digest(imageRef string) (string, error) {
	return digestOf(p.ImageDir, imageRef)
}

// Config reports the image configuration this node recorded.
func (p *UblkProvider) Config(imageRef string) (*Config, error) {
	return cachedConfig(p.ImageDir, imageRef)
}

// Compile-time proof that this satisfies the interface noded drives.
//
// An assertion rather than trust: the interface has six methods and a provider missing one
// would still build until something assigned it, at which point the error names the
// assignment rather than the gap.
var _ Provider = (*UblkProvider)(nil)
