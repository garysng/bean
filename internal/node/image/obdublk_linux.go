//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// prepareUblk assembles a sandbox's rootfs from overlaybd layers served by a ublk
// device this process drives, rather than by the overlaybd daemon over tcmu.
//
// The layer chain is resolved by exactly the same code as the tcmu route: sharing by
// digest, the object store, conversion when a layer is not here yet. What differs is
// everything below the chain -- the layers are read by lsmtStack in this process, the
// writable layer is a sparse file with an ownership bitmap instead of an overlaybd
// writable layer, and the device is created by writing io_uring commands rather than by
// assembling a SCSI fabric per sandbox.
//
// Kept in its own file rather than branching inside Prepare because the two routes
// share only their first step. Interleaving them would produce a function where half
// the statements are reached on one path.
func (p *OverlaybdProvider) prepareUblk(ctx context.Context, sandboxID, imageRef string, sizeMiB int64, opts PrepareOptions) (rootfs *Rootfs, err error) {
	// The configuration is checked before the hardware, so a node misconfigured *and*
	// without ublk reports the thing an operator can fix. The other order hides it: the
	// control device is absent on every machine without ublk, and that error would
	// mask a flag combination that is wrong regardless of the kernel.
	//
	// Lazy pull cannot be served here because the reader in this process has no HTTP
	// range-read: a lazily read layer is a URL, and lsmtStack needs a file it can seek.
	// Refused rather than quietly converting locally -- lazy pull is the property that
	// was asked for, and not providing it silently is invisible downstream.
	if p.LazyPull {
		return nil, errors.New("image: --fc-overlaybd-lazy-pull cannot be served over ublk: " +
			"the ublk reader needs each layer as a local file, while lazy pull means " +
			"range-reading blobs over HTTP. Use one or the other")
	}

	ctrl, err := p.ublkControl()
	if err != nil {
		return nil, err
	}

	// A restore resolves its whole filesystem from the snapshot's sealed chain, the
	// same identity the tcmu route uses; a cold start resolves from the image
	// reference. Either way what comes back is a list of read-only lowers, which is
	// all this route needs -- the sandbox's writes go to its own overlay rather than
	// into a layer.
	//
	// Publish is not honoured here. Publishing is an S3 upload of converted layers, and
	// a create that has no object store configured cannot do it; the flag is documented
	// as a no-op on such backends, and this route is one until the reader can read
	// published layers remotely.
	var lowers []obdLayer
	if opts.FSManifestDigest != "" {
		lowers, err = p.snapshotFSLowers(ctx, opts.FSManifestDigest)
	} else {
		lowers, _, _, err = p.lowersFor(ctx, imageRef, false)
	}
	if err != nil {
		return nil, err
	}

	paths, err := localLayerPaths(lowers, imageRef)
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

	stack, closeStack, err := openLSMTStack(paths)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = closeStack() })

	// Sized from the request, then raised to fit the filesystem in the layers.
	//
	// The two figures are decided independently -- the device from what the caller
	// asked for, the filesystem by whatever vsizeForImage estimated when the base was
	// converted -- so they agree only by accident. Raised rather than refused for the
	// reason the tcmu path documents at length: the base's size is an artifact of how
	// the image was sealed, refusing would make a small sandbox impossible on an image
	// whose floor is larger, and the extra is sparse so it costs nothing until written.
	//
	// The failure this avoids is remote from its cause: the guest kernel refuses to
	// mount its own root with "bad geometry: block count N exceeds size of device", and
	// the caller sees a 20-second agent-health timeout naming none of it.
	size := sizeMiB << 20
	if stack.virtualSize > size {
		size = stack.virtualSize
	}

	overlayPath := filepath.Join(dir, "overlay.img")
	backend, err := newLSMTBackendOverStack(stack, closeStack, overlayPath, size)
	if err != nil {
		return nil, err
	}
	// The backend owns the stack from here, so the earlier cleanup would double-close.
	// Replaced rather than appended: Close closes both.
	cleanup[len(cleanup)-1] = func() { _ = backend.Close() }

	// The overlay is left empty even on a restore. A snapshot's filesystem travels as a
	// sealed layer resolved into the lowers above, not as extents replayed into the
	// writable layer, so there is nothing to seed.
	dev, err := attachUblk(ctrl, backend, backend.size)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = dev.detach() })

	p.mu.Lock()
	p.ublkAttached[sandboxID] = dev
	p.mu.Unlock()

	// Firecracker resolves drive paths relative to its working directory, so the device
	// is linked in under a local name.
	link := filepath.Join(dir, "rootfs.img")
	if err := os.Symlink(dev.Device, link); err != nil {
		return nil, fmt.Errorf("image: link rootfs device: %w", err)
	}

	return &Rootfs{
		Device: link,
		// The overlay is what a checkpoint captures: it holds everything this sandbox
		// changed, and the layers are reproducible from their digests.
		Writable: overlayPath,
		release: func() error {
			p.mu.Lock()
			delete(p.ublkAttached, sandboxID)
			p.mu.Unlock()

			var errs []error
			// Device first, then the backend: the device is still serving reads from the
			// backend until it is detached, and closing the files under a live device
			// would fail those reads instead of ending them.
			detachStart := time.Now()
			if err := dev.detach(); err != nil {
				errs = append(errs, err)
			}
			obsPhase(ctx, "obd_ublk_detach", time.Since(detachStart))

			if err := backend.Close(); err != nil {
				errs = append(errs, err)
			}

			rmStart := time.Now()
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove sandbox dir: %w", err))
			}
			obsPhase(ctx, "obd_ublk_remove_dir", time.Since(rmStart))
			return errors.Join(errs...)
		},
	}, nil
}

// localLayerPaths returns each layer's file, refusing a chain that names one only by
// digest.
//
// A chain resolved against the object store can reference a layer the daemon would
// range-read. The reader here needs a file it can seek, so such a chain is refused with
// the layer and the image named: an operator's next step is to prewarm that image on
// this node, and neither an index nor a digest alone says which image to prewarm.
func localLayerPaths(lowers []obdLayer, imageRef string) ([]string, error) {
	paths := make([]string, 0, len(lowers))
	for i, l := range lowers {
		if l.File == "" {
			return nil, fmt.Errorf("image: layer %d of %s (%s) is only available remotely, "+
				"and the ublk reader needs a local file: prewarm this image on this node, "+
				"or run this node without --fc-ublk", i, imageRef, l.Digest)
		}
		paths = append(paths, l.File)
	}
	return paths, nil
}

func (p *OverlaybdProvider) ublkControl() (*ublkControl, error) {
	p.ublkCtrlOnce.Do(func() {
		p.ublkCtrl, p.ublkCtrlErr = openUblkControl()
	})
	return p.ublkCtrl, p.ublkCtrlErr
}
