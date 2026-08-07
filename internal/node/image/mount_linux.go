//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// A container runtime needs a directory tree; a microVM needs a block device.
// Providers here produce the latter, because that is what Firecracker attaches, so
// the container tier has to bridge the two.
//
// This is a helper rather than a field on Rootfs on purpose. Being mounted is not a
// property of a rootfs -- it is something one caller needs done to it -- and adding a
// Dir field would leave every fc create carrying a value it never sets and every
// reader wondering which of the two to trust.

// MountDir mounts a prepared rootfs and returns the directory plus a release that
// undoes both the mount and the rootfs itself.
//
// The returned release supersedes Rootfs.Release: calling it unmounts and then frees
// the device, in that order. A caller that used Rootfs.Release directly would detach
// a device the kernel still has mounted, which leaves the mount serving a device that
// no longer exists rather than failing.
//
// dir is created under the sandbox's own directory rather than in a temp dir, so a
// leaked mount is attributable to a sandbox and reclaim can find it.
func MountDir(rootfs *Rootfs, sandboxDir string) (dir string, release func() error, err error) {
	if rootfs == nil || rootfs.Device == "" {
		return "", nil, errors.New("image: cannot mount a rootfs with no device")
	}
	dir = filepath.Join(sandboxDir, "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("image: create mount point: %w", err)
	}

	// The device may be a symlink -- the overlaybd provider links its TCMU device in
	// under a local name because Firecracker resolves drive paths relative to its
	// working directory. mount(2) does not follow one, so it is resolved here.
	device := rootfs.Device
	if resolved, rerr := filepath.EvalSymlinks(device); rerr == nil {
		device = resolved
	}

	// ext4 named explicitly rather than probed. Every rootfs this package produces is
	// ext4 -- convert_linux.go runs mkfs.ext4, and the overlaybd base layer is created
	// with --mkfs -- so a filesystem that is not ext4 means something upstream is
	// wrong, and failing here says so at the mount rather than at first read.
	if err := syscall.Mount(device, dir, "ext4", 0, ""); err != nil {
		return "", nil, fmt.Errorf("image: mount %s at %s: %w", device, dir, err)
	}

	return dir, func() error {
		var errs []error
		if uerr := unmountDir(dir); uerr != nil {
			errs = append(errs, uerr)
			// The device is deliberately not released after a failed unmount: doing so
			// would detach a device the kernel still has mounted. Reported instead, so
			// reclaim finds a mount to clean up rather than a device that vanished.
			return errors.Join(errs...)
		}
		if rerr := rootfs.Release(); rerr != nil {
			errs = append(errs, rerr)
		}
		return errors.Join(errs...)
	}, nil
}

// unmountDir detaches a mount, treating "was not mounted" as success so a release
// running twice is not an error.
func unmountDir(dir string) error {
	err := syscall.Unmount(dir, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.ENOENT):
		// Not a mount point, or already gone.
		return nil
	case errors.Is(err, syscall.EBUSY):
		// Something still has it open. Lazy unmount detaches the tree now and lets the
		// kernel finish when the last reference goes, which is what a teardown wants:
		// the alternative is a mount that stays until reboot because one process was
		// slow to exit.
		if lerr := syscall.Unmount(dir, syscall.MNT_DETACH); lerr != nil {
			return fmt.Errorf("image: unmount %s (busy, detach failed): %w", dir, lerr)
		}
		return nil
	default:
		return fmt.Errorf("image: unmount %s: %w", dir, err)
	}
}
