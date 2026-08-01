//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Committing turns a sandbox's filesystem into a base image others can start
// from. It is the shortest path for "set the environment up interactively, then
// freeze it", and unlike a snapshot the result is an ordinary image: no memory
// state, no binding to the runtime tier that produced it.
//
// The work is a merge rather than a copy. A sandbox's rootfs is a shared
// read-only base plus its own copy-on-write store, so committing means writing
// out the base with the changed blocks applied — which is what the kernel's
// snapshot target already computes.

// Committer seals a sandbox's rootfs into a new base image.
type Committer struct {
	// ImageDir is where committed images land, alongside pulled ones: a
	// committed image is a base image like any other.
	ImageDir string
	// WorkDir holds partial work, on the same filesystem as ImageDir so the
	// final move is atomic.
	WorkDir string
}

// Commit writes the sandbox's current filesystem as the image named by tag.
//
// device is the sandbox's assembled rootfs — the merged view, not the base and
// not the copy-on-write store. Reading through it is what makes this correct
// regardless of how the provider assembled it: a copy-on-write device, a plain
// file, or something later.
func (c *Committer) Commit(ctx context.Context, device, tag string) (path string, err error) {
	if device == "" {
		return "", errors.New("image: rootfs device required")
	}
	name, err := refToFilename(tag)
	if err != nil {
		return "", err
	}

	final := filepath.Join(c.ImageDir, name+imageSuffix)
	if _, err := os.Stat(final); err == nil {
		// Images are immutable: committing over an existing tag would change
		// what every sandbox already started from it believes about its own
		// base. A new tag is the way to make a new image.
		return "", fmt.Errorf("image: %s already exists", tag)
	}

	for _, dir := range []string{c.ImageDir, c.WorkDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("image: create %s: %w", dir, err)
		}
	}

	tmpPath, err := reserveTempName(c.WorkDir, name)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if err = copyDeviceSparse(ctx, device, tmpPath); err != nil {
		return "", err
	}

	// A filesystem captured from a running sandbox was not unmounted, so its
	// journal may hold uncommitted transactions and its state is "dirty".
	// Replaying and clearing that now means a sandbox starting from this image
	// mounts a clean filesystem rather than recovering on every boot.
	if err = fsck(tmpPath); err != nil {
		return "", err
	}

	if err = os.Rename(tmpPath, final); err != nil {
		return "", fmt.Errorf("image: publish committed image: %w", err)
	}
	if err = recordRef(c.ImageDir, tag); err != nil {
		return "", fmt.Errorf("image: record reference: %w", err)
	}
	return final, nil
}

// copyDeviceSparse reads a block device into a sparse file.
//
// The device has no holes to detect — it presents its full size — so zero
// detection is done on the content. A committed image is mostly empty
// filesystem, so this is the difference between an image costing its
// provisioned size and costing what is in it.
func copyDeviceSparse(ctx context.Context, device, dest string) error {
	src, err := os.Open(device)
	if err != nil {
		return fmt.Errorf("image: open rootfs device: %w", err)
	}
	defer src.Close()

	size, err := blockDeviceSize(device)
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("image: create committed image: %w", err)
	}
	defer out.Close()
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("image: size committed image: %w", err)
	}

	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if !allZero(buf[:n]) {
				if _, werr := out.WriteAt(buf[:n], offset); werr != nil {
					return werr
				}
			}
			offset += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("image: read rootfs device: %w", rerr)
		}
	}
	return out.Sync()
}

// blockDeviceSize reports a device's size in bytes. os.Stat reports zero for a
// block device, so the size comes from the kernel — in 512-byte sectors, which
// is why this converts rather than returning them directly.
func blockDeviceSize(device string) (int64, error) {
	// A regular file (the FileProvider case) reports its size normally.
	if info, err := os.Stat(device); err == nil && info.Mode().IsRegular() {
		return info.Size(), nil
	}
	sectors, err := deviceSectors(device)
	if err != nil {
		return 0, err
	}
	return sectors * 512, nil
}

// fsck replays the journal and clears the dirty bit on a filesystem captured
// while mounted. Exit codes 1 and 2 mean errors were found and fixed, which is
// the expected outcome here rather than a failure.
func fsck(path string) error {
	cmd := exec.Command("e2fsck", "-p", "-f", path)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 1, 2:
			// Fixed. Code 2 asks for a reboot, which is meaningless for an
			// image file that nothing has mounted.
			return nil
		}
	}
	return fmt.Errorf("image: e2fsck on committed image: %v: %s",
		err, strings.TrimSpace(out.String()))
}
