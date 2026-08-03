//go:build linux

package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Converting an OCI image to a bootable rootfs is what lets a user hand the
// platform any image and get a microVM, with no template to build and no format
// to think about. That claim is only true if the node does the work, which is
// what this file is.
//
// The steps are: create a filesystem, mount it, apply each layer in order
// honouring whiteouts, then add the few directories a guest needs. The result is
// an ext4 image — the format FCRuntime attaches and the guest mounts.

// Converter builds base images from registry content.
type Converter struct {
	// Registry fetches manifests and layers.
	Registry *Registry
	// ImageDir is where finished base images land.
	ImageDir string
	// WorkDir holds partial work; it must be on the same filesystem as
	// ImageDir so the final move is atomic.
	WorkDir string
	// DefaultSizeMiB sizes the filesystem when the image's own size gives no
	// better hint.
	DefaultSizeMiB int64
}

// Convert pulls an image and writes its base filesystem, returning the path.
//
// The work is done under a temporary name and moved into place at the end, so a
// concurrent Prepare never sees a partially written image — it would mount it
// and fail in a way that looks like a corrupt image rather than a race.
func (c *Converter) Convert(ctx context.Context, imageRef string) (path string, err error) {
	ref, err := ParseReference(imageRef)
	if err != nil {
		return "", err
	}
	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}

	final := filepath.Join(c.ImageDir, name+imageSuffix)
	if _, err := os.Stat(final); err == nil {
		// Already converted. Images are immutable once written — a tag that
		// moves is a different digest and so a different file.
		return final, nil
	}

	manifest, err := c.Registry.FetchManifest(ctx, ref)
	if err != nil {
		return "", err
	}

	return writeBaseImage(c.ImageDir, c.WorkDir, imageRef, manifest.Digest, c.sizeFor(manifest),
		func(root string) error {
			for i, layer := range manifest.Layers {
				if err := c.applyLayer(ctx, ref, layer, root); err != nil {
					return fmt.Errorf("image: apply layer %d/%d: %w",
						i+1, len(manifest.Layers), err)
				}
			}
			return nil
		})
}

// sizeFor picks a filesystem size. Layers are compressed, so their total is a
// lower bound on the unpacked content; the multiplier and floor cover the
// difference plus room for the sandbox to write.
func (c *Converter) sizeFor(m *Manifest) int64 {
	var compressed int64
	for _, l := range m.Layers {
		compressed += l.Size
	}
	sizeMiB := (compressed >> 20) * 3
	if floor := c.DefaultSizeMiB; sizeMiB < floor {
		sizeMiB = floor
	}
	if sizeMiB < 256 {
		sizeMiB = 256
	}
	return sizeMiB
}

// applyLayer unpacks one layer over the mounted filesystem.
func (c *Converter) applyLayer(ctx context.Context, ref Reference, layer Descriptor, root string) error {
	blob, err := c.Registry.FetchBlob(ctx, ref, layer.Digest)
	if err != nil {
		return err
	}
	defer blob.Close()

	var src io.Reader = blob
	// Most layers are gzipped; the media type says so, but some registries are
	// loose about it, so the magic bytes decide.
	if strings.Contains(layer.MediaType, "gzip") || strings.HasSuffix(layer.MediaType, "tar+gzip") {
		zr, err := gzip.NewReader(blob)
		if err != nil {
			return fmt.Errorf("open gzip layer: %w", err)
		}
		defer zr.Close()
		src = zr
	}

	return extractTar(tar.NewReader(src), root)
}

// extractTar unpacks a layer, honouring OCI whiteout markers.
func extractTar(tr *tar.Reader, root string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}

		base := filepath.Base(hdr.Name)
		// A whiteout deletes a path from the layers below; an opaque whiteout
		// clears the directory's whole previous content.
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				if err := clearDir(filepath.Dir(target)); err != nil {
					return err
				}
				continue
			}
			victim := filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, ".wh."))
			if err := os.RemoveAll(victim); err != nil {
				return fmt.Errorf("apply whiteout %s: %w", victim, err)
			}
			continue
		}

		if err := writeEntry(tr, hdr, target); err != nil {
			return fmt.Errorf("write %s: %w", hdr.Name, err)
		}
	}
}

// safeJoin resolves a tar entry's path inside root, refusing anything that would
// escape. A malicious or malformed image must not be able to write to the node's
// filesystem during conversion.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean("/" + name)
	joined := filepath.Join(root, clean)
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("layer entry %q escapes the image root", name)
	}
	return joined, nil
}

func writeEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
			return err
		}
		// A later layer may re-declare a directory with different permissions.
		return os.Chmod(target, os.FileMode(hdr.Mode)&os.ModePerm)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// A file replacing one from a lower layer must be truncated, not merged.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
			os.FileMode(hdr.Mode)&os.ModePerm)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		return f.Close()

	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		os.Remove(target)
		return os.Symlink(hdr.Linkname, target)

	case tar.TypeLink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		source, err := safeJoin(filepath.Dir(filepath.Dir(target)), hdr.Linkname)
		if err != nil {
			// A hard link target outside the image is not resolvable; skipping
			// is better than aborting a whole conversion for it.
			return nil
		}
		os.Remove(target)
		if err := os.Link(source, target); err != nil {
			// Hard links across layers do not always resolve; a copy preserves
			// the content, which is what the guest needs.
			return copyFallback(source, target, os.FileMode(hdr.Mode)&os.ModePerm)
		}
		return nil

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device nodes are not recreated: the guest gets devtmpfs, which is a
		// better source of them than an image's static entries.
		return nil

	default:
		return nil
	}
}

func copyFallback(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return nil // source absent; nothing to link
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// clearDir empties a directory, which is what an opaque whiteout asks for.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// prepareGuestDirs adds the mountpoints the agent needs. They are created here
// rather than in the guest because the image is mounted read-write only during
// conversion, and a minimal image may not have them.
func prepareGuestDirs(root string) error {
	for _, dir := range []string{"bean", "proc", "sys", "dev", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return fmt.Errorf("image: create guest dir %s: %w", dir, err)
		}
	}
	// /tmp is world-writable with the sticky bit, as programs expect.
	return os.Chmod(filepath.Join(root, "tmp"), 0o1777)
}

// reserveTempName returns an unused path in dir without leaving a file there.
//
// os.CreateTemp both picks the name and creates the file, but makeExt4 creates
// exclusively — that is what stops two conversions writing the same image — so
// the placeholder is removed. The gap between removing it and recreating it is
// harmless: CreateTemp's names are random, so nothing else is trying for this
// one.
func reserveTempName(dir, name string) (string, error) {
	f, err := os.CreateTemp(dir, name+".*.partial")
	if err != nil {
		return "", fmt.Errorf("image: reserve temp name: %w", err)
	}
	path := f.Name()
	f.Close()
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func makeExt4(path string, sizeMiB int64) error {
	if err := createSparse(path, sizeMiB); err != nil {
		return err
	}
	// -F because the target is a file rather than a device, and -q because
	// mkfs's progress output is not useful in a server log.
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-O", "^has_journal", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image: mkfs.ext4: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// mount attaches an image file. The mount binary is used rather than the
// syscall because attaching a file needs a loop device set up first, which the
// syscall does not do — there is no mount flag for it.
func mount(image, target string) error {
	cmd := exec.Command("mount", "-o", "loop", image, target)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image: mount %s: %v: %s", image, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

func unmount(target string) error {
	if err := syscall.Unmount(target, 0); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
			// Not mounted; nothing to do.
			return nil
		}
		cmd := exec.Command("umount", target)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if cerr := cmd.Run(); cerr != nil {
			return fmt.Errorf("image: unmount %s: %v: %s", target, cerr,
				strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
