//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Lazy pull is served here now, by reading the layer over range requests instead of
	// from a file. It used to be refused: this process had no range client, so a layer
	// available only as a URL had nothing to open. What closed the gap is that every
	// reader below takes io.ReaderAt, so a range-reading base substitutes for a file
	// without the format code knowing.
	// Not gated on a registry client here: a layer published to the object store is read by
	// URL without one, and that is the common lazy-pull case. remoteLayerFetcher refuses
	// the registry case with no client, which is where the requirement actually is.

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
	// The conversion has to be reported back the same way the tcmu route reports it, or
	// the template the control plane created for this reference never leaves PENDING.
	//
	// Found on hardware: this route returned a nil Conversion, so a first create worked
	// and the template stayed PENDING with an empty FS digest forever. That is worse than
	// it sounds -- a template stuck in PENDING is one nothing can be created from, so the
	// reference was effectively single-use.
	//
	// Publish is passed through rather than forced false. Publishing is an S3 upload and a
	// node with no object store cannot do it, but that is the blob store's decision to
	// refuse, not this route's to pre-empt: hardcoding false here made the two transports
	// disagree about what a create means, which is exactly what a shared code path is for.
	var lowers []obdLayer
	var manifest *Manifest
	var published bool
	if opts.FSManifestDigest != "" {
		lowers, err = p.snapshotFSLowers(ctx, opts.FSManifestDigest)
	} else {
		lowers, manifest, published, err = p.lowersFor(ctx, imageRef, opts.Publish)
	}
	if err != nil {
		return nil, err
	}

	var conversion *ConversionResult
	if published && manifest != nil && manifest.Digest != "" {
		conversion = &ConversionResult{
			ManifestDigest: manifest.Digest,
			OCIDigest:      manifest.Digest,
			SizeBytes:      chainSize(lowers),
			LayerDigests:   layerDigestsOf(lowers),
		}
		// The image's config rides along so the template records it, as a build reports
		// the config it recovered. Failing to resolve it is not fatal to the create --
		// the sandbox still boots from the chain -- so the template just records none.
		if ref, perr := ParseReference(imageRef); perr == nil {
			if cfg, cerr := p.imageConfig(ctx, ref, imageRef, manifest); cerr == nil {
				conversion.Config = cfg
			}
		}
	}

	sources, err := p.layerSources(ctx, lowers, imageRef)
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

	stack, closeStack, err := openLSMTStackFrom(sources)
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
	// The backend is registered alongside the device because a checkpoint seals from its
	// ownership bitmap rather than from a writable layer on disk. Under the same lock so the
	// two cannot disagree about whether this sandbox exists.
	p.sealableUblk[sandboxID] = backend
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
		Writable:   overlayPath,
		Conversion: conversion,
		release: func() error {
			p.mu.Lock()
			delete(p.ublkAttached, sandboxID)
			delete(p.sealableUblk, sandboxID)
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

// layerSources turns a resolved chain into what openLSMTStackFrom reads.
//
// A layer on disk is read from the file; one available only by digest is read over range
// requests, which is what lazy pull means on this route. Local wins when both are possible:
// a file this node already holds costs nothing per read, while a remote layer costs a round
// trip per uncached chunk, so preferring the file is not a policy choice.
//
// Without lazy pull a remote-only layer is still refused, and refused by name -- an
// operator's next step is to prewarm that image here, and neither an index nor a bare digest
// says which image to prewarm.
func (p *OverlaybdProvider) layerSources(ctx context.Context, lowers []obdLayer, imageRef string) ([]layerSource, error) {
	srcs := make([]layerSource, 0, len(lowers))
	for i, l := range lowers {
		if l.File != "" {
			srcs = append(srcs, layerSource{Path: l.File, Label: l.Digest})
			continue
		}
		if !p.LazyPull {
			return nil, fmt.Errorf("image: layer %d of %s (%s) is only available remotely, "+
				"and this node is not configured for lazy pull: prewarm this image on this "+
				"node, or start it with --fc-overlaybd-lazy-pull", i, imageRef, l.Digest)
		}

		// The size has to be known before the first read, because both the tar wrapper and
		// the LSMT trailer are located from the *end* of the blob. A manifest carries it, so
		// the usual case costs no extra request; the fetcher falls back to a HEAD when it
		// does not.
		fetcher, err := p.remoteLayerFetcher(l, imageRef)
		if err != nil {
			return nil, fmt.Errorf("image: layer %d of %s (%s): %w", i, imageRef, l.Digest, err)
		}
		size := l.Size
		if size <= 0 {
			size, err = fetcher.Size(ctx)
			if err != nil {
				return nil, fmt.Errorf("image: size of layer %d of %s (%s): %w",
					i, imageRef, l.Digest, err)
			}
		}
		srcs = append(srcs, layerSource{
			// Deliberately not ctx. A reader built here serves the device for the
			// sandbox's whole life, while ctx belongs to this create and is cancelled the
			// moment it returns -- after which every read fails with `context canceled`,
			// reaching the guest as EIO and reported as filesystem corruption. The reads
			// during the create succeed, which is what made this look like a bad region of
			// the layer rather than a lifetime bug.
			Remote:     newRemoteBlobReader(context.WithoutCancel(ctx), fetcher, p.chunks()),
			RemoteSize: size,
			Label:      l.Digest,
		})
	}
	return srcs, nil
}

// remoteLayerFetcher builds the range client for one remote layer.
//
// Two shapes of source, and they are not interchangeable. A layer published to the object
// store carries a *base URL* in RepoBlobURL -- "http://host/bucket/blobs" -- and its blob
// sits at that URL plus "/<digest>", which is the convention overlaybd's own daemon
// follows. A layer still in a registry is addressed by a reference, and its blob is at
// /v2/<repo>/blobs/<digest>.
//
// Treating the first as the second is what my first version did, and it produced
// `https://http/v2//127.0.0.1:9000/bean-obd-layers/blobs/blobs/sha256:...`: the scheme
// parsed as a host, a /v2/ path inserted, and "blobs" appearing twice. Worth spelling out,
// because the resulting error named DNS rather than the mistake.
func (p *OverlaybdProvider) remoteLayerFetcher(l obdLayer, imageRef string) (rangeFetcher, error) {
	if strings.HasPrefix(l.RepoBlobURL, "http://") || strings.HasPrefix(l.RepoBlobURL, "https://") {
		return &urlRangeFetcher{
			url:    strings.TrimSuffix(l.RepoBlobURL, "/") + "/" + l.Digest,
			digest: l.Digest,
			size:   l.Size,
		}, nil
	}

	if p.Registry == nil {
		return nil, errors.New("no registry client configured, and this layer is not in an " +
			"object store")
	}
	src := l.RepoBlobURL
	if src == "" {
		src = imageRef
	}
	ref, err := ParseReference(src)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", src, err)
	}
	return newRegistryRangeFetcher(p.Registry, ref, l.Digest, l.Size), nil
}

// chunks is the node's shared cache of fetched layer chunks.
//
// One per provider rather than per create: a node running many sandboxes from one image
// reads the same chunks of the same layers, and a per-create cache would hold a copy each
// and evict them independently, which is the case the cache exists for.
func (p *OverlaybdProvider) chunks() *chunkCache {
	p.chunkOnce.Do(func() { p.chunkCache = newChunkCache(p.ChunkCacheBytes) })
	return p.chunkCache
}

func (p *OverlaybdProvider) ublkControl() (*ublkControl, error) {
	p.ublkCtrlOnce.Do(func() {
		p.ublkCtrl, p.ublkCtrlErr = openUblkControl()
	})
	return p.ublkCtrl, p.ublkCtrlErr
}
