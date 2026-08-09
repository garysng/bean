//go:build linux

package image

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/garysng/bean/internal/logging"
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
	// ImageDir holds the per-image metadata files naming which layers an image is made
	// of. Shared with the other providers so `Cached` reports one view of an
	// image regardless of which backend holds it.
	ImageDir string
	// Registry resolves manifests and fetches layer blobs.
	Registry *Registry
	// Builder converts OCI layers into overlaybd ones.
	Builder *OverlaybdBuilder
	// DefaultSizeMiB bounds the writable layer when a spec does not.
	DefaultSizeMiB int64
	// LazyPull has the daemon range-read layers over HTTP instead of this node
	// holding them.
	//
	// Off by default. A locally converted layer is a file this node owns; a lazily
	// read one makes every block read depend on the store still being reachable and
	// still serving that digest. That is the right trade for a large image used once
	// and the wrong one for a node expected to keep working while its object store
	// is down, so it is a deployment decision rather than a default.
	LazyPull bool

	// Blobs publishes sealed layers where the daemon can read them, which is what
	// makes lazy pull possible on an image that arrived as ordinary OCI: the layer is
	// converted once, published under its digest, and every later create -- on this
	// node or any other reading the same store -- references it without converting.
	//
	// Nil restricts lazy pull to images whose registry blobs are already sealed
	// overlaybd layers, which almost nothing is.
	Blobs BlobStore

	// Index records which layers make up an image and what a tag points at, so a node
	// that has never seen an image can resolve it from the store rather than the
	// registry. Written by prewarm, read by create.
	//
	// Nil leaves the store a layer cache: the blobs are there, but nothing says which
	// of them form an image, so every create still resolves against the registry.
	Index ImageIndex

	mu sync.Mutex
	// attached tracks live devices so teardown can find them, and so a leaked
	// configfs object is attributable.
	attached map[string]*tcmuDevice

	// layers collapses concurrent conversions of the same layer digest. Keyed by
	// digest to match sealedLayerPath, so one flight corresponds to one output
	// file.
	layers layerFlight

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
	// The blob URL is hoisted to the config root because that is the only place
	// overlaybd reads it: __open_ro_remote uses conf.repoBlobUrl() while taking dir,
	// digest and size from the layer. Written per-layer it is silently dropped, and
	// the create fails with "empty repoBlobUrl for remote layer" in the daemon's log
	// and a bare ENOENT at the caller.
	blobURL, err := chainBlobURL(lowers)
	if err != nil {
		return nil, err
	}
	cfg := &obdConfig{
		RepoBlobURL: blobURL,
		Lowers:      lowers,
		Upper:       obdUpper{Data: data, Index: index},
		ResultFile:  filepath.Join(dir, "result"),
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := writeConfig(cfgPath, cfg); err != nil {
		return nil, err
	}

	// The filesystem has to be grown to match the device before the device exists,
	// because the two sizes are decided independently and only agree by accident.
	//
	// The device is sized from what the caller asked for. The filesystem lives in the
	// base layer and was sized by vsizeForImage when that layer was converted -- an
	// estimate from the image's compressed layers, with a 2 GB floor. Neither knows
	// about the other, so a sandbox asking for less than the estimate gets a device
	// smaller than the filesystem inside it, and the kernel refuses to mount its own
	// root: "bad geometry: block count 524288 exceeds size of device (262144 blocks)".
	//
	// Measured on hardware: 1024 MiB failed, 2048 and 4096 worked. The default
	// --default-disk-mib is 2048, exactly the floor, which is why every end-to-end run
	// passed while any smaller disk was unusable.
	//
	// Growing rather than shrinking the device to fit is what the caller asked for: a
	// configured 20 GB should be 20 GB of writable space. The resize writes through the
	// chain into the *writable* layer, so the shared base is untouched and this is
	// per-sandbox work.
	if err := p.Builder.resizeToGB(ctx, cfgPath, vsizeGB); err != nil {
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

// lowersFor resolves an image to its read-only layer chain for a create.
//
// A create never publishes. It looks, in order, for a layer this node already has, a
// layer the object store already has, and only then converts -- and a conversion it is
// forced into stays local. Publication belongs to Prewarm, which is the call that
// exists to do slow work ahead of time; doing it here put an S3 upload of tens of MiB
// on the latency path of a sandbox whose bytes were already sitting on the node's
// disk, to benefit some later create that may never come.
//
// Layers are shared by digest, so an image sharing a base with one already here
// resolves the shared layers for free. That is the whole point of the backend: the
// flattening path pays for the shared bytes once per image.
func (p *OverlaybdProvider) lowersFor(ctx context.Context, imageRef string) ([]obdLayer, error) {
	lowers, err := p.resolve(ctx, imageRef, false)
	if errors.Is(err, errRemoteParent) {
		// A partly published image: some layers are in the store and a later one is
		// not, so it has to be converted and has no local parent to apply over. This
		// happens when a prewarm was interrupted part-way through an image.
		//
		// Retried with remote reads off, which converts the whole chain locally. That
		// spends the download the remote levels exist to avoid -- but the alternative
		// is refusing a create that can plainly succeed, and a sandbox that starts
		// slowly beats one that does not start.
		slog.Warn("image is only partly published; converting the whole chain locally",
			logging.KeyImage, imageRef, logging.KeyError, err)
		return p.resolveLocal(ctx, imageRef)
	}
	return lowers, err
}

// resolveLocal resolves an image without consulting the store, so every layer is
// either already here or converted.
func (p *OverlaybdProvider) resolveLocal(ctx context.Context, imageRef string) ([]obdLayer, error) {
	return p.walk(ctx, imageRef, resolveOpts{})
}

// resolveOpts selects which of the lookup levels a walk may use.
type resolveOpts struct {
	// publish uploads every local layer, and suppresses remote reads: a conversion
	// needs its parents as local files, so a chain being published must be local
	// throughout.
	publish bool
	// remote allows level 2, reading a published layer instead of converting it.
	remote bool
}

// errRemoteParent means a layer has to be converted but sits over one available only
// remotely -- a partly published image. Sentinel rather than a string match because
// lowersFor recovers from it by converting the chain locally.
var errRemoteParent = errors.New("image: layer must be converted but its parent is only available remotely")

// resolve walks an image's layers for a create, consulting the store.
func (p *OverlaybdProvider) resolve(ctx context.Context, imageRef string, publish bool) ([]obdLayer, error) {
	return p.walk(ctx, imageRef, resolveOpts{publish: publish, remote: !publish})
}

// resolveManifest answers "which layers is this image made of", from the registry when
// it can and from this node's own record when it cannot.
//
// The fallback exists because without it overlaybd is *less* available than the
// flattening backend it is meant to improve on. DevMapperProvider.Prepare is purely
// local -- it looks for a converted file and never opens a socket -- so a node with the
// registry unreachable still starts every image it has cached. Every overlaybd create
// fetched the manifest first, so the same node started nothing.
//
// A prewarm does not fall back. Its purpose is to bring the node up to date with the
// registry, and a prewarm that "succeeded" against a local record would report an image
// as freshly warmed while having asked nobody. Failing is the honest answer.
//
// What the fallback gives up is tag freshness: the recorded chain is what the tag
// resolved to last time. That is the same trade warm snapshots already make by keying on
// digest, and it is announced rather than silent, because a create that quietly serves a
// stale tag is the kind of failure that succeeds and produces the wrong answer.
func (p *OverlaybdProvider) resolveManifest(ctx context.Context, ref Reference, imageRef string, opts resolveOpts) (*Manifest, error) {
	// A digest reference needs no registry at all once recorded: the digest *is* the
	// answer a manifest fetch would return, and an OCI manifest is immutable for a
	// given digest, so the recorded chain is the same chain rather than a stale guess.
	//
	// Deliberately not extended to tags. A tag is a mutable pointer, so answering it
	// from the record would pin the image at whatever it resolved to last time and
	// never notice an update -- a create that succeeds and quietly runs the wrong
	// image. The manifest fetch a tag costs is a few KB of JSON, not layer data, so
	// there is little to win and correctness to lose.
	if !opts.publish && ref.Digest != "" {
		if rec, err := cachedRecord(p.ImageDir, imageRef); err == nil && rec != nil && len(rec.Layers) > 0 {
			return manifestFromRecord(rec), nil
		}
	}

	// The store, if it has been told what this image is. This is what lets a node that
	// has never seen the image resolve it without the registry -- the local record above
	// only helps a node that already pulled it once.
	//
	// Skipped for a prewarm, whose job is to refresh the store from the registry: reading
	// its own previous answer would make it a no-op that reports success.
	if !opts.publish {
		if m := p.storedManifest(ctx, ref); m != nil {
			return m, nil
		}
	}

	manifest, err := p.Registry.FetchManifest(ctx, ref)
	if err == nil {
		return manifest, nil
	}
	if opts.publish {
		return nil, err
	}
	// The caller giving up is not the registry being unreachable, and must not be
	// answered from a stale record.
	if ctx.Err() != nil {
		return nil, err
	}
	rec, recErr := cachedRecord(p.ImageDir, imageRef)
	if recErr != nil || rec == nil || len(rec.Layers) == 0 {
		// Nothing recorded, so there is no second opinion to offer and the registry
		// error is the real one. Returned unwrapped so the caller sees the cause.
		return nil, err
	}
	slog.Warn("registry unreachable; starting from the layer chain this node recorded, "+
		"which is what the reference resolved to when it was last pulled",
		logging.KeyImage, imageRef, "digest", rec.Digest, logging.KeyError, err)
	return manifestFromRecord(rec), nil
}

// recordInStore tells the store what this image is made of, so another node can resolve
// it without the registry.
//
// Failures warn rather than propagate. The image is prewarmed either way -- its layers are
// published and this node holds them -- and failing the job over an index write would turn
// a lost optimisation into a lost prewarm. What it costs is that other nodes keep going to
// the registry, which is the behaviour they had before this existed.
//
// The manifest is written before the tag, and that order matters: a tag pointing at a
// manifest that is not there yet is a dangling reference a reader would resolve to nothing,
// while a manifest nothing points at is merely unreferenced until the tag lands.
func (p *OverlaybdProvider) recordInStore(ctx context.Context, ref Reference, manifest *Manifest, lowers []obdLayer, cfg *Config) {
	if p.Index == nil || manifest.Digest == "" {
		return
	}
	layers := make([]StoredLayer, 0, len(lowers))
	for i, l := range lowers {
		// Sizes come from the resolved chain, not the manifest: what the store holds is
		// the sealed layer, whose length differs from the OCI blob's, and a reader
		// range-reads against whatever this says.
		mediaType := ""
		if i < len(manifest.Layers) {
			mediaType = manifest.Layers[i].MediaType
		}
		layers = append(layers, StoredLayer{Digest: l.Digest, Size: l.Size, MediaType: mediaType})
	}
	stored := &StoredManifest{Digest: manifest.Digest, Layers: layers, Config: cfg}
	if err := p.Index.PutManifest(ctx, manifest.Digest, stored); err != nil {
		slog.Warn("could not record the image manifest in the store; other nodes will "+
			"resolve this image against the registry",
			"digest", manifest.Digest, logging.KeyError, err)
		return
	}
	if ref.Tag == "" {
		// A digest reference has no tag to point, and needs none: the digest is already
		// the manifest key.
		return
	}
	if err := p.Index.PutTag(ctx, ref, manifest.Digest); err != nil {
		slog.Warn("could not record the tag pointer in the store; other nodes will "+
			"resolve this tag against the registry",
			logging.KeyImage, ref.Repository+":"+ref.Tag, logging.KeyError, err)
	}
}

// PublishBuiltRootfs seals a freshly built rootfs tar into a base overlaybd layer,
// publishes it to the shared store and records a one-layer manifest and tag, so a
// node that did not build the image can start from it exactly as it starts from a
// prewarmed OCI image. It is how a Dockerfile build enters the same lazy-pull path
// pulled images use (docs/s3-storage.md section 8.5, Phase 2).
//
// tarPath is the decompressed rootfs BuildKit exported. A built image is a single
// base layer -- there is no chain to diff against -- so the tar is applied with no
// parents, which is what makes buildLayer format a filesystem (--mkfs) into it.
//
// The layer is keyed by the sha256 of the rootfs tar rather than a registry digest,
// which a built image has none of: it is stable (same tar, same key, so a rebuild
// dedupes) and it is only an identity, never read as content. The sealed size that a
// remote create range-reads against is the sealed file's own length, recorded
// separately by recordInStore from the resolved chain.
//
// Returns an empty overlaybdRef with no error when this provider has no store
// configured: the build still succeeds as a node-local image, which is the historical
// single-node behaviour, and the caller reports the empty ref upward. Publication and
// recording failures warn rather than propagate, exactly as the prewarm path's do -- a
// build is not failed over a cache-warming miss.
func (p *OverlaybdProvider) PublishBuiltRootfs(ctx context.Context, tarPath, imageRef string, sizeMiB int64) (overlaybdRef string, layerDigests []string, sealed int64, err error) {
	if p.Blobs == nil || p.Index == nil {
		return "", nil, 0, nil
	}
	ref, err := ParseReference(imageRef)
	if err != nil {
		return "", nil, 0, err
	}

	digest, err := sha256OfFile(tarPath)
	if err != nil {
		return "", nil, 0, fmt.Errorf("image: digest built rootfs: %w", err)
	}

	// vsize mirrors the create path's sizing: the tar is uncompressed, so its bytes
	// are the content, and the layer's virtual size is grown from there. A built
	// image's requested SizeMiB, when set, is the floor the sandbox will want.
	vsizeGB := (sizeMiB >> 10) + 1
	if vsizeGB < 2 {
		vsizeGB = 2
	}

	path, err := p.Builder.buildLayer(ctx, tarPath, digest, vsizeGB, nil)
	if err != nil {
		return "", nil, 0, fmt.Errorf("image: seal built rootfs: %w", err)
	}
	sealed = sealedSize(path)

	if ok, err := p.publish(ctx, path, digest); err != nil {
		return "", nil, 0, fmt.Errorf("image: publish built layer: %w", err)
	} else if !ok {
		// The store declined the upload (and warned). The layer is sealed locally, so
		// this node can start the image, but no other can -- report no ref rather than
		// one that promises a reach the artifact does not have.
		return "", nil, sealed, nil
	}

	// A synthetic manifest keyed by the sealed layer, recorded exactly as a prewarm
	// records a converted chain. The lower's Size is the sealed length, which is what a
	// remote reader range-reads against. No config: a flat rootfs carries none, matching
	// what build.go already documents about built images.
	manifest := &Manifest{Digest: digest, Layers: []Descriptor{{Digest: digest, Size: sealed}}}
	lowers := []obdLayer{{File: path, Digest: digest, Size: sealed}}
	// An empty but non-nil config, recorded so a node that did not build the image can
	// resolve it fully offline. A built image carries no OCI config -- buildctl exports a
	// flat rootfs tar, so there is no ENV/ENTRYPOINT/CMD, which is what build.go documents
	// about built images running only what the caller asks. The distinction that matters
	// here is non-nil vs nil: imageConfig treats a nil stored config as "go ask the
	// registry", and a built tag is in no registry, so a cache-cleared create would fail
	// with "no reachable config and none recorded". A recorded empty config is the honest
	// value and the one that lets the offline resolve complete.
	p.recordInStore(ctx, ref, manifest, lowers, &Config{})

	return digest, []string{digest}, sealed, nil
}

// sha256OfFile streams a file through sha256 and returns an OCI-style digest. Used to
// key a built layer, which has no registry digest of its own.
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// storedManifest resolves a reference through the object store, or nil if the store
// cannot answer.
//
// Nil rather than an error for every miss: the caller's next move is the registry, and an
// error would only turn a store that has not been prewarmed into a failed create. A
// *corrupt* answer is different and does propagate, because using it would produce a
// device whose reads fail for reasons naming zfile structure.
func (p *OverlaybdProvider) storedManifest(ctx context.Context, ref Reference) *Manifest {
	if p.Index == nil {
		return nil
	}
	digest := ref.Digest
	if digest == "" {
		// A tag has to be resolved through the store's own pointer. This is the step
		// that makes the store a source rather than a cache -- and the step that makes
		// bean, not the upstream registry, the authority on what the tag means until the
		// next prewarm.
		var err error
		digest, err = p.Index.GetTag(ctx, ref)
		if err != nil {
			slog.Warn("stored tag pointer is unusable; resolving against the registry",
				logging.KeyImage, ref.Repository+":"+ref.Tag, logging.KeyError, err)
			return nil
		}
		if digest == "" {
			return nil
		}
	}
	stored, err := p.Index.GetManifest(ctx, digest)
	if err != nil {
		slog.Warn("stored manifest is unusable; resolving against the registry",
			"digest", digest, logging.KeyError, err)
		return nil
	}
	if stored == nil {
		return nil
	}

	layers := make([]Descriptor, 0, len(stored.Layers))
	for _, l := range stored.Layers {
		layers = append(layers, Descriptor{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
	}
	// storedConfig carries the config through to imageConfig, which would otherwise go
	// to the registry for it and undo the point of resolving offline.
	return &Manifest{Digest: digest, Layers: layers, storedConfig: stored.Config}
}

// manifestFromRecord rebuilds a manifest from what the node wrote down.
//
// The config descriptor is left empty rather than invented: the recorded config is the
// parsed form, not the blob, so there is no digest to name. imageConfig reads that
// absence as "use the recorded config", which is the only source available anyway.
func manifestFromRecord(rec *ImageRecord) *Manifest {
	layers := make([]Descriptor, 0, len(rec.Layers))
	for _, l := range rec.Layers {
		layers = append(layers, Descriptor{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
	}
	return &Manifest{Digest: rec.Digest, Layers: layers}
}

// imageConfig fetches the image's OCI configuration, falling back to the copy this node
// recorded.
//
// Needed alongside the manifest fallback rather than instead of it: the config lives in
// its own blob, so a manifest answered from the local record still leaves a registry
// fetch on the path. Without this the offline create gets further and then fails on the
// config blob instead -- the same outcome, one step later.
//
// The recorded config is exactly the right thing to use here. It came from this digest
// when the image was pulled, and an OCI config is immutable for a given digest, so the
// local copy is not a stale approximation but the same bytes.
func (p *OverlaybdProvider) imageConfig(ctx context.Context, ref Reference, imageRef string, manifest *Manifest) (*Config, error) {
	// Already answered by whatever resolved the manifest -- the object store carries the
	// config alongside the layer list precisely so this does not become a registry call.
	if manifest.storedConfig != nil {
		return manifest.storedConfig, nil
	}
	// A manifest rebuilt from the local record carries no config descriptor, so there
	// is nothing to fetch and the recorded config is the only source.
	if manifest.Config.Digest != "" {
		cfg, err := p.Registry.FetchConfig(ctx, ref, manifest)
		if err == nil {
			return cfg, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		rec, recErr := cachedRecord(p.ImageDir, imageRef)
		if recErr != nil || rec == nil || rec.Config == nil {
			return nil, err
		}
		return rec.Config, nil
	}
	rec, err := cachedRecord(p.ImageDir, imageRef)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Config == nil {
		// An image whose config is neither fetchable nor recorded would start with no
		// ENV, ENTRYPOINT or CMD -- a sandbox that boots and then does the wrong thing.
		// Refused instead.
		return nil, fmt.Errorf("image: %s has no reachable config and none recorded", imageRef)
	}
	return rec.Config, nil
}

// walk resolves an image to a layer chain under the given options.
//
// One implementation for create and prewarm on purpose: they must agree on what a
// layer chain is, or an image would assemble differently depending on which call
// arrived first.
func (p *OverlaybdProvider) walk(ctx context.Context, imageRef string, opts resolveOpts) ([]obdLayer, error) {
	publish := opts.publish
	if p.Registry == nil {
		return nil, errors.New("image: overlaybd needs a registry")
	}
	ref, err := ParseReference(imageRef)
	if err != nil {
		return nil, err
	}
	manifest, err := p.resolveManifest(ctx, ref, imageRef, opts)
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
		// The layer's sealed form is what a device is built from, whether it is read
		// locally or over HTTP. Only the first layer carries a filesystem; the rest
		// are changes over it. Sized generously because the virtual size bounds what
		// the layer can hold and costs nothing when unused -- the files are sparse.
		vsizeGB := int64(1)
		if i == 0 {
			vsizeGB = p.vsizeForImage(manifest)
		}

		// Level 0: the registry blob is already a sealed overlaybd layer, so there is
		// nothing to convert and nothing to publish -- point the daemon at the
		// registry. Checked first because it is the only case where no store and no
		// local copy are needed at all.
		if p.LazyPull && isOverlaybdLayer(layer.MediaType) {
			lowers = append(lowers, obdLayer{
				Digest:      layer.Digest,
				Size:        layer.Size,
				RepoBlobURL: repoBlobURL(ref),
				Dir:         p.layerCacheDir(layer.Digest),
			})
			continue
		}

		// Level 1: this node has the sealed layer. Referenced as a file, not as a
		// remote blob with a cache: the bytes are here, and routing them through the
		// daemon's HTTP path would make a local read depend on the store being up.
		if path := p.Builder.sealedLayerPath(layer.Digest); exists(path) {
			// A prewarm publishes it even though it converted nothing. Otherwise a node
			// that had already converted the image would prewarm to a no-op and the
			// layer would sit on one node's disk, unreachable to the rest -- which is
			// the opposite of what prewarming is for.
			if publish {
				if _, err := p.publish(ctx, path, layer.Digest); err != nil {
					return nil, fmt.Errorf("image: layer %d/%d: %w", i+1, len(manifest.Layers), err)
				}
			}
			// Size is the sealed file's own length, not the manifest's figure for the
			// original OCI layer: sealing recompresses, so the two differ (measured
			// 48859648 against a declared 29780765). Wrong here, a local layer still
			// works because overlaybd reads the file, but a remote one is range-read
			// against the declared length and either stops short or reads past the end.
			lowers = append(lowers, obdLayer{File: path, Digest: layer.Digest, Size: sealedSize(path)})
			continue
		}

		// Level 2: the store has it, so this node reads blocks on demand instead of
		// converting. This is the level that makes a create cheap on a node that has
		// never seen the image, and the reason publication is worth doing at all.
		//
		// Off when publishing, and off in the local fallback. A prewarm's output is a
		// *local* chain: conversion applies a layer's tar over its parents as files, so
		// taking a remote reference for an early layer leaves a later one with no local
		// parent to build on. That is not hypothetical -- it is what a prewarm did on a
		// node whose earlier layers were already in the store, failing with the level-3
		// refusal below against its own publication.
		if opts.remote {
			if remote, ok := p.remoteLayer(ctx, layer.Digest); ok {
				lowers = append(lowers, remote)
				continue
			}
		}

		// Level 3: convert. The layers already resolved are this one's parents -- an
		// OCI layer is a diff, so its tar has to be applied over them rather than into
		// an empty filesystem.
		parents := make([]string, 0, len(lowers))
		for _, l := range lowers {
			parents = append(parents, p.Builder.sealedLayerPath(l.Digest))
		}
		if remoteParent := firstRemote(lowers); remoteParent != "" {
			// A conversion needs its parents as local files, and a layer resolved
			// remotely has none. Reported as errRemoteParent so lowersFor can retry the
			// whole image locally, rather than converting the parents here -- which
			// would leave this walk half remote and half local with no record of why.
			return nil, fmt.Errorf("%w: layer %d/%d of %s over parent %s",
				errRemoteParent, i+1, len(manifest.Layers), imageRef, shortHash(remoteParent))
		}
		path, err := p.materialiseLayer(ctx, ref, layer, vsizeGB, parents)
		if err != nil {
			return nil, fmt.Errorf("image: layer %d/%d: %w", i+1, len(manifest.Layers), err)
		}
		lower := obdLayer{File: path, Digest: layer.Digest, Size: sealedSize(path)}

		if publish {
			// Published so later creates -- here or on any node reading the same store
			// -- reach level 2 instead of converting. The layer is still referenced as
			// a local file for this caller: it has the bytes, and switching to a remote
			// reference would make the read depend on the upload it just did.
			if _, err := p.publish(ctx, path, layer.Digest); err != nil {
				return nil, fmt.Errorf("image: layer %d/%d: %w", i+1, len(manifest.Layers), err)
			}
		}
		lowers = append(lowers, lower)
	}

	// The image's own configuration has to be recorded here for the same reason the
	// flattening path records it during conversion: this is the only moment a
	// registry is in reach, and without it the guest never learns the image's ENV,
	// ENTRYPOINT, CMD or WORKDIR. Skipping it on this backend would make an image
	// start differently depending on which provider a node happens to use, and
	// nothing would report the difference.
	cfg, err := p.imageConfig(ctx, ref, imageRef, manifest)
	if err != nil {
		return nil, err
	}

	if publish {
		// Recorded in the store only after every layer has been published, so the index
		// never advertises an image whose layers are not all there. A node resolving a
		// half-published manifest would reach the level-3 refusal instead, having been
		// told the image was available.
		p.recordInStore(ctx, ref, manifest, lowers, cfg)
	}

	// The chain's size is recorded because there is no single file to stat: an
	// overlaybd image is layer files shared with other images. Summing the layers
	// over-reports a shared base -- two images each claim it -- but the alternative
	// was reporting nothing, which made the image invisible to the scheduler.
	//
	// The chain itself is recorded so a later create on this node can rebuild it
	// without asking the registry what the tag resolves to.
	if err := recordRef(p.ImageDir, ImageRecord{
		Ref:       imageRef,
		Digest:    manifest.Digest,
		Config:    cfg,
		SizeBytes: chainSize(lowers),
		Layers:    recordedLayers(manifest),
	}); err != nil {
		return nil, fmt.Errorf("image: record reference: %w", err)
	}
	return lowers, nil
}

// recordedLayers is the manifest's layer list, reduced to what a later resolution
// needs.
//
// Taken from the manifest rather than from the resolved chain because it has to
// describe the *image*, not this node's current arrangement of it: the resolved layers
// carry local paths and cache directories that stop being true as soon as a layer is
// reclaimed. Media type is kept because it decides whether a blob needs converting at
// all -- an already-sealed overlaybd layer takes level 0.
func recordedLayers(m *Manifest) []RecordedLayer {
	out := make([]RecordedLayer, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, RecordedLayer{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
	}
	return out
}

// materialiseLayer converts one OCI layer to overlaybd format, skipping the work
// if another image already brought it here, and joining an in-progress conversion
// rather than starting a second one.
func (p *OverlaybdProvider) materialiseLayer(ctx context.Context, ref Reference, layer Descriptor, vsizeGB int64, parents []string) (string, error) {
	if path := p.Builder.sealedLayerPath(layer.Digest); exists(path) {
		return path, nil
	}
	// Keyed by digest, not by image: two images sharing a base name the same
	// digest, and that is exactly the case the reference-level dedup in
	// PullingProvider cannot see.
	return p.layers.do(ctx, layer.Digest, func(ctx context.Context) (string, error) {
		// Re-checked inside the flight because the leader we might have waited
		// behind may have just produced it.
		if path := p.Builder.sealedLayerPath(layer.Digest); exists(path) {
			return path, nil
		}
		return p.convertLayer(ctx, ref, layer, vsizeGB, parents)
	})
}

// convertLayer does the fetch-decompress-build for one layer. Callers reach it
// through materialiseLayer, which handles the dedup.
func (p *OverlaybdProvider) convertLayer(ctx context.Context, ref Reference, layer Descriptor, vsizeGB int64, parents []string) (string, error) {

	// The work directory has to exist before the layer is staged into it. buildLayer
	// creates it, but staging happens first, so relying on that ordering meant every
	// create failed on a node whose image directory was new -- and passed on one where
	// an earlier run had left the directory behind, which is why this survived a
	// working end-to-end run.
	if err := os.MkdirAll(p.Builder.WorkDir, 0o700); err != nil {
		return "", fmt.Errorf("image: create work dir: %w", err)
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

	return p.Builder.buildLayer(ctx, staged.Name(), layer.Digest, vsizeGB, parents)
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

// repoBlobURL is the registry's blob endpoint for a reference, which is where the
// daemon reads an image whose blobs are already sealed overlaybd layers.
func repoBlobURL(ref Reference) string {
	return fmt.Sprintf("https://%s/v2/%s/blobs", ref.Host, ref.Repository)
}

// layerCacheDir is where a remotely read layer's fetched blocks are kept.
//
// Per layer and named by digest, so two images sharing a layer share its cache --
// the same reason sealed layers are named by digest.
func (p *OverlaybdProvider) layerCacheDir(digest string) string {
	dir := filepath.Join(p.LayerDir, "cache", sanitiseDigest(digest))
	// Created eagerly: the daemon does not create it, and a missing directory turns
	// into every read going to the network with only a warning in overlaybd's log.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// Not fatal. Losing the cache costs bandwidth, not correctness, and the
		// alternative -- failing the create -- is worse for a sandbox that would
		// otherwise have started.
		slog.Warn("overlaybd layer cache unavailable; reads will go to the network",
			logging.KeyError, err, "dir", dir)
		return ""
	}
	return dir
}

// remoteLayer reports a layer the store already holds, as a reference the daemon
// range-reads.
//
// Only under lazy pull: without it a node is expected to own its layers, and reading
// them over HTTP would make every block read depend on the store being reachable --
// the trade LazyPull's comment describes as a deployment decision.
//
// The size comes from the store rather than the manifest, because the published blob
// is the sealed layer and the manifest describes the original OCI one.
func (p *OverlaybdProvider) remoteLayer(ctx context.Context, digest string) (obdLayer, bool) {
	if !p.LazyPull || p.Blobs == nil {
		return obdLayer{}, false
	}
	size, ok, err := p.Blobs.Stat(ctx, digest)
	if err != nil || !ok {
		// Unknown is treated as absent: converting locally always works, and failing a
		// create because a cache could not be consulted would be worse.
		return obdLayer{}, false
	}
	return obdLayer{
		Digest:      digest,
		Size:        size,
		RepoBlobURL: p.Blobs.BlobURL(),
		Dir:         p.layerCacheDir(digest),
	}, true
}

// chainBlobURL is the one blob URL a chain's remote layers share, or "" if every layer
// is local.
//
// A single value because overlaybd's config has one: the URL is read from the config
// root, not per layer. Two remote layers disagreeing therefore cannot be expressed, and
// are refused here rather than serialised into a config where one of them would be
// fetched from the other's prefix -- a wrong-data failure, not a visible one.
//
// In practice they agree: remote layers are either all from bean's object store or all
// from one image's registry repository.
func chainBlobURL(lowers []obdLayer) (string, error) {
	url := ""
	for _, l := range lowers {
		if l.RepoBlobURL == "" {
			continue
		}
		if url != "" && l.RepoBlobURL != url {
			return "", fmt.Errorf("image: chain mixes blob sources %q and %q, which "+
				"overlaybd's single config-level repoBlobUrl cannot express", url, l.RepoBlobURL)
		}
		url = l.RepoBlobURL
	}
	return url, nil
}

// chainSize is what an image's layers amount to, for the cache listing.
//
// At least 1 for a chain that resolved entirely remotely, where nothing is on this
// node's disk: the listing treats zero as "no size recorded, stat the file instead",
// and there is no file. A remote image is cached in the sense the scheduler cares
// about -- this node can start it without a conversion.
func chainSize(lowers []obdLayer) int64 {
	var total int64
	for _, l := range lowers {
		total += l.Size
	}
	if total <= 0 {
		return 1
	}
	return total
}

// firstRemote reports the digest of the first layer that has no local file, or "".
func firstRemote(lowers []obdLayer) string {
	for _, l := range lowers {
		if l.File == "" {
			return l.Digest
		}
	}
	return ""
}

// publish uploads a sealed layer so later creates can read it remotely, and reports
// whether the layer is now available that way.
//
// A false return is not an error: it means this create should use the local file. The
// reasons are all recoverable -- no store configured, or an upload that failed while
// the local copy is right here -- and failing a create because a cache could not be
// warmed would trade a working sandbox for a future optimisation.
func (p *OverlaybdProvider) publish(ctx context.Context, path, digest string) (bool, error) {
	if p.Blobs == nil {
		return false, nil
	}
	if _, ok, _ := p.Blobs.Stat(ctx, digest); ok {
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open sealed layer: %w", err)
	}
	defer f.Close()
	if err := p.Blobs.Put(ctx, digest, sealedSize(path), f); err != nil {
		slog.Warn("could not publish overlaybd layer; using the local copy",
			logging.KeyError, err, "digest", digest)
		return false, nil
	}
	return true, nil
}

// sealedSize is the sealed layer's own length, which is what overlaybd needs to
// range-read it -- not the size the manifest reports for the original OCI layer.
// Sealing recompresses, so the two differ, and a wrong size makes the daemon read
// past the end or stop short.
func sealedSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// Prewarm converts an image's layers without creating a sandbox, and publishes what
// it converted.
//
// This is the only call that publishes. It is the one whose latency nobody is waiting
// on, so it is where the upload belongs -- and it is what turns conversion from
// per-node work into work the first node does once for every node reading the same
// store. A fleet that never prewarms still functions; every node just converts for
// itself.
func (p *OverlaybdProvider) Prewarm(ctx context.Context, imageRef string) error {
	_, err := p.resolve(ctx, imageRef, true)
	return err
}

func (p *OverlaybdProvider) Cached() (map[string]CachedImage, error) {
	return p.cache.get(p.ImageDir)
}

func (p *OverlaybdProvider) Digest(imageRef string) (string, error) {
	return digestOf(p.ImageDir, imageRef)
}

// Config reports the image configuration this node recorded, written by lowersFor
// when the image's layers were resolved.
func (p *OverlaybdProvider) Config(imageRef string) (*Config, error) {
	return cachedConfig(p.ImageDir, imageRef)
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
