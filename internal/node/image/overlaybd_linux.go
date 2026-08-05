//go:build linux

package image

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// OverlaybdProvider assembles a sandbox rootfs from overlaybd layers rather than
// from a flattened ext4.
//
// The difference from DevMapperProvider is what happens to an image's layers.
// convert_linux.go unpacks every layer into one filesystem, which makes any image
// bootable with no template but discards the layer structure: two images sharing a
// base cost two full copies. Here the layers stay separate and are named by their
// OCI digest, so a shared base is stored once no matter how many images reference
// it -- measured at 3.1x less disk for a SWE-bench-shaped set (GitHub #32).
//
// The second difference is that a layer need not be local at all. overlaybd
// range-reads blobs from the registry as blocks are touched, so first use of a
// large image does not wait for a full download: 19.6% of one layer's bytes were
// enough to mount and read a file.
type OverlaybdProvider struct {
	// BaseDir holds per-sandbox writable layers and configs.
	BaseDir string
	// LayerDir holds sealed read-only layers, shared across images.
	LayerDir string
	// ImageDir holds the per-image sidecars naming which layers an image is made
	// of. Shared with the other providers so `Cached` reports one view of an
	// image regardless of which backend holds it.
	ImageDir string
	// Registry resolves manifests and fetches layer blobs.
	Registry *Registry
	// Builder converts OCI layers into overlaybd ones.
	Builder *OverlaybdBuilder
	// DefaultSizeMiB bounds the writable layer when a spec does not.
	DefaultSizeMiB int64
	// LazyPull leaves layers in the registry and lets overlaybd range-read them
	// instead of converting them locally.
	//
	// Off by default. A locally converted layer is a file this node owns; a lazily
	// pulled one makes every block read depend on the registry still being
	// reachable and still serving that digest. That is the right trade for a large
	// image used once, and the wrong one for a node expected to keep working while
	// the registry is down, so it is a deployment decision rather than a default.
	LazyPull bool

	mu sync.Mutex
	// attached tracks live devices so teardown can find them, and so a leaked
	// configfs object is attributable.
	attached map[string]*tcmuDevice

	cache cachedRefs
}

func NewOverlaybdProvider(baseDir, layerDir, imageDir string, reg *Registry, builder *OverlaybdBuilder, defaultSizeMiB int64) *OverlaybdProvider {
	return &OverlaybdProvider{
		BaseDir:        baseDir,
		LayerDir:       layerDir,
		ImageDir:       imageDir,
		Registry:       reg,
		Builder:        builder,
		DefaultSizeMiB: defaultSizeMiB,
		attached:       map[string]*tcmuDevice{},
	}
}

func (p *OverlaybdProvider) Name() string { return "overlaybd" }

// Available reports whether this host can run the provider, so a node fails to
// start rather than accepting placements it cannot honour.
func (p *OverlaybdProvider) Available() error {
	if err := tcmuAvailable(); err != nil {
		return err
	}
	if p.Builder == nil {
		return errors.New("image: overlaybd builder not configured")
	}
	return p.Builder.available()
}

// Prepare assembles one sandbox's rootfs: the image's layers as read-only lowers,
// plus a sparse writable layer belonging to this sandbox alone.
func (p *OverlaybdProvider) Prepare(ctx context.Context, sandboxID, imageRef string, opts PrepareOptions) (rootfs *Rootfs, err error) {
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

	lowers, err := p.lowersFor(ctx, imageRef)
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

	// The writable layer is sized in GB because that is overlaybd's unit; rounding
	// up means a request for less than a gigabyte still gets a usable layer rather
	// than a zero-sized one.
	vsizeGB := (sizeMiB + 1023) / 1024
	data, index, err := p.Builder.createWritable(ctx, dir, vsizeGB)
	if err != nil {
		return nil, err
	}

	// A restore's contents have to be in place before the device is assembled:
	// overlaybd opens the writable layer when the backstore is enabled, and bytes
	// written afterwards are not in the index it read.
	if opts.SeedWritable != nil {
		if err := opts.SeedWritable(data); err != nil {
			return nil, fmt.Errorf("image: seed writable layer: %w", err)
		}
	}

	cfgPath := filepath.Join(dir, "overlaybd.json")
	cfg := &obdConfig{
		Lowers:     lowers,
		Upper:      obdUpper{Data: data, Index: index},
		ResultFile: filepath.Join(dir, "result"),
	}
	if p.LazyPull {
		cfg.RepoBlobURL, err = p.repoBlobURL(imageRef)
		if err != nil {
			return nil, err
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := writeConfig(cfgPath, cfg); err != nil {
		return nil, err
	}

	// The serial is what keeps multipathd from merging this device with another and
	// serving the wrong image's data (see setSerial). It has to be a hash of the
	// sandbox id rather than the id itself: the kernel keeps only the hex digits
	// when it derives the WWID, so "sbx-alpha" and "sbx-a-l-p-h-a" would collide.
	// deviceSerial produces hex only, so what the caller sees is what the kernel
	// registers.
	dev, err := attachTCMU(DMName(sandboxID), cfgPath, deviceSerial(sandboxID), vsizeGB<<30)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = dev.detach() })

	p.mu.Lock()
	p.attached[sandboxID] = dev
	p.mu.Unlock()

	// Firecracker resolves drive paths relative to its working directory, so the
	// device is linked in under a local name.
	link := filepath.Join(dir, "rootfs.img")
	if err := os.Symlink(dev.Device, link); err != nil {
		return nil, fmt.Errorf("image: link rootfs device: %w", err)
	}

	return &Rootfs{
		Device: link,
		// The writable layer is what a checkpoint captures: it holds everything
		// this sandbox changed, and the lowers are reproducible from their digests.
		Writable: data,
		release: func() error {
			p.mu.Lock()
			delete(p.attached, sandboxID)
			p.mu.Unlock()

			var errs []error
			if err := dev.detach(); err != nil {
				errs = append(errs, err)
			}
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove sandbox dir: %w", err))
			}
			return errors.Join(errs...)
		},
	}, nil
}

// lowersFor resolves an image to its read-only layer chain, converting layers this
// node has not seen.
//
// Layers are shared by digest, so an image sharing a base with one already here
// converts only what is genuinely new. That is the whole point of the backend: the
// flattening path pays for the shared bytes once per image.
func (p *OverlaybdProvider) lowersFor(ctx context.Context, imageRef string) ([]obdLayer, error) {
	if p.Registry == nil {
		return nil, errors.New("image: overlaybd needs a registry")
	}
	ref, err := ParseReference(imageRef)
	if err != nil {
		return nil, err
	}
	manifest, err := p.Registry.FetchManifest(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("image: %s has no layers", imageRef)
	}
	if len(manifest.Layers) > maxLayers {
		return nil, fmt.Errorf("image: %s has %d layers, overlaybd allows %d",
			imageRef, len(manifest.Layers), maxLayers)
	}

	lowers := make([]obdLayer, 0, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		// Lazy pull hands overlaybd the digest and lets it range-read. Nothing is
		// converted, so first use costs no local work at all.
		if p.LazyPull {
			lowers = append(lowers, obdLayer{Digest: layer.Digest, Size: layer.Size})
			continue
		}
		// Only the first layer carries a filesystem; the rest are changes over it.
		// Sized generously because the virtual size bounds what the layer can hold
		// and costs nothing when unused -- the files are sparse.
		vsizeGB := int64(1)
		if i == 0 {
			vsizeGB = p.vsizeForImage(manifest)
		}
		path, err := p.materialiseLayer(ctx, ref, layer, vsizeGB)
		if err != nil {
			return nil, fmt.Errorf("image: layer %d/%d: %w", i+1, len(manifest.Layers), err)
		}
		lowers = append(lowers, obdLayer{File: path, Digest: layer.Digest, Size: layer.Size})
	}

	if err := recordRef(p.ImageDir, imageRef, manifest.Digest); err != nil {
		return nil, fmt.Errorf("image: record reference: %w", err)
	}
	return lowers, nil
}

// materialiseLayer converts one OCI layer to overlaybd format, skipping the work
// if another image already brought it here.
func (p *OverlaybdProvider) materialiseLayer(ctx context.Context, ref Reference, layer Descriptor, vsizeGB int64) (string, error) {
	if path := p.Builder.sealedLayerPath(layer.Digest); exists(path) {
		return path, nil
	}

	blob, err := p.Registry.FetchBlob(ctx, ref, layer.Digest)
	if err != nil {
		return "", err
	}
	defer blob.Close()

	// overlaybd-apply reads a tar, not a tar.gz, so a compressed layer is
	// decompressed to a staging file first. The magic bytes are not consulted
	// here (unlike applyLayer) because the media type is all a manifest gives and
	// a wrong guess produces a tar error rather than silent corruption.
	staged, err := os.CreateTemp(p.Builder.WorkDir, "layer.*.tar")
	if err != nil {
		return "", fmt.Errorf("image: stage layer: %w", err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()

	var src io.Reader = blob
	if strings.Contains(layer.MediaType, "gzip") {
		zr, err := gzip.NewReader(blob)
		if err != nil {
			return "", fmt.Errorf("image: open gzip layer: %w", err)
		}
		defer zr.Close()
		src = zr
	}
	if _, err := io.Copy(staged, src); err != nil {
		return "", fmt.Errorf("image: read layer: %w", err)
	}
	if err := staged.Close(); err != nil {
		return "", err
	}

	return p.Builder.buildLayer(ctx, staged.Name(), layer.Digest, vsizeGB)
}

// vsizeForImage sizes the base layer's filesystem.
//
// Layers are compressed, so their total is a lower bound on the unpacked content;
// the multiplier covers the difference plus room for the sandbox to write. Sparse
// files mean an overestimate costs nothing, which is why this leans generous --
// unlike the ext4 path, where the same estimate running short means ENOSPC
// part-way through a conversion.
func (p *OverlaybdProvider) vsizeForImage(m *Manifest) int64 {
	var compressed int64
	for _, l := range m.Layers {
		compressed += l.Size
	}
	gb := (compressed*4)>>30 + 1
	if gb < 2 {
		gb = 2
	}
	return gb
}

// repoBlobURL is where overlaybd fetches blobs for a lazily pulled image.
func (p *OverlaybdProvider) repoBlobURL(imageRef string) (string, error) {
	ref, err := ParseReference(imageRef)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s/v2/%s/blobs", ref.Host, ref.Repository), nil
}

// Prewarm converts an image's layers without creating a sandbox.
//
// Under lazy pull this is close to a no-op by design: there is nothing to
// materialise, and the point of the backend is that first use does not wait.
func (p *OverlaybdProvider) Prewarm(ctx context.Context, imageRef string) error {
	_, err := p.lowersFor(ctx, imageRef)
	return err
}

func (p *OverlaybdProvider) Cached() (map[string]CachedImage, error) {
	return p.cache.get(p.ImageDir)
}

func (p *OverlaybdProvider) Digest(imageRef string) (string, error) {
	return digestOf(p.ImageDir, imageRef)
}

// CommitSandbox seals a sandbox's writable layer into a shareable read-only one.
//
// This is where the backend pays off a second time: the dm-snapshot path reads out
// a whole ext4 because a copy-on-write store is not an OCI layer, whereas here the
// writable layer already is one and sealing it is a metadata operation over bytes
// that are already in the right format.
func (p *OverlaybdProvider) CommitSandbox(ctx context.Context, sandboxID, dest string) error {
	p.mu.Lock()
	_, ok := p.attached[sandboxID]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("image: sandbox %s is not attached", sandboxID)
	}
	dir := filepath.Join(p.BaseDir, sandboxID)
	return p.Builder.sealWritable(ctx,
		filepath.Join(dir, "writable.data"),
		filepath.Join(dir, "writable.index"),
		dest)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
