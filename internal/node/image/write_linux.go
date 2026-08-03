//go:build linux

package image

import (
	"fmt"
	"os"
	"path/filepath"
)

// Writing a base image is the same work whether the content came from registry
// layers or from a build: make a filesystem, fill it, add the directories the
// guest needs, publish it under its reference. Only the filling differs, so that
// is the part callers supply.

// writeBaseImage builds an image at the reference's canonical path, calling fill
// with the mounted filesystem's root.
//
// The image is assembled under a temporary name and moved into place at the end,
// so a concurrent Prepare never mounts a half-written filesystem — which would
// fail in a way that looks like a corrupt image rather than a race.
// digest is the manifest digest imageRef resolved to, or "" for an image with no
// manifest -- a build's output, or a commit of a sandbox's filesystem. It is
// recorded so a warm snapshot can be keyed on the image's identity rather than on
// the name it was fetched under; see recordRef.
func writeBaseImage(imageDir, workDir, imageRef, digest string, sizeMiB int64,
	fill func(root string) error) (path string, err error) {

	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}
	final := filepath.Join(imageDir, name+imageSuffix)

	for _, dir := range []string{imageDir, workDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("image: create %s: %w", dir, err)
		}
	}

	tmpPath, err := reserveTempName(workDir, name)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if err = makeExt4(tmpPath, sizeMiB); err != nil {
		return "", err
	}

	mnt, err := os.MkdirTemp(workDir, "mnt.*")
	if err != nil {
		return "", fmt.Errorf("image: create mountpoint: %w", err)
	}
	defer os.RemoveAll(mnt)

	if err = mount(tmpPath, mnt); err != nil {
		return "", err
	}
	// Unmounting must happen before the move, and must happen even when filling
	// fails, or the image file stays busy and the work directory cannot be
	// cleaned up.
	defer func() {
		if uerr := unmount(mnt); uerr != nil && err == nil {
			err = uerr
		}
	}()

	if err = fill(mnt); err != nil {
		return "", err
	}
	if err = prepareGuestDirs(mnt); err != nil {
		return "", err
	}

	if err = unmount(mnt); err != nil {
		return "", err
	}
	if err = os.Rename(tmpPath, final); err != nil {
		return "", fmt.Errorf("image: publish base image: %w", err)
	}

	// The sidecar records which reference this file came from, which is how the
	// node reports what it holds. It is written after the image, so a sidecar
	// never advertises an image that is not yet usable.
	if err = recordRef(imageDir, imageRef, digest); err != nil {
		return "", fmt.Errorf("image: record reference: %w", err)
	}
	return final, nil
}
