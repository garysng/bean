//go:build linux

package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Turning an OCI layer into an overlaybd one is a three-step pipeline, and the
// steps are separate binaries rather than one because each produces an artifact
// worth keeping: create makes an empty writable layer, apply writes a tar into it,
// commit seals it into something read-only and shareable.
//
// What this buys over convert_linux.go's flatten-to-ext4: the layers stay
// separate. Two images sharing a base share its bytes on disk instead of each
// paying for a full copy, and a sealed layer can be pushed to a registry and
// range-read by another node rather than pulled whole.

// OverlaybdBuilder converts OCI layers into sealed overlaybd layers.
type OverlaybdBuilder struct {
	// BinDir holds the overlaybd binaries. Empty resolves them on PATH.
	BinDir string
	// LayerDir is where sealed layers land, named by their OCI digest.
	LayerDir string
	// WorkDir holds partial work, on the same filesystem as LayerDir so
	// publishing a finished layer is an atomic rename.
	WorkDir string
	// ServiceConfig is overlaybd's daemon config, which apply needs in order to
	// resolve its cache and credential settings.
	ServiceConfig string
}

// NewOverlaybdBuilder configures the layer-conversion pipeline.
//
// serviceConfig defaults to overlaybd's own installed path, which is where its
// cache and credential settings live. overlaybd-apply reads it, so a wrong path
// means layers are converted without the node's registry credentials.
func NewOverlaybdBuilder(binDir, layerDir, workDir string) *OverlaybdBuilder {
	return &OverlaybdBuilder{
		BinDir:        binDir,
		LayerDir:      layerDir,
		WorkDir:       workDir,
		ServiceConfig: defaultServiceConfig,
	}
}

// defaultServiceConfig is where overlaybd's packaging puts its daemon config.
const defaultServiceConfig = "/etc/overlaybd/overlaybd.json"

func (b *OverlaybdBuilder) bin(name string) string {
	if b.BinDir == "" {
		return name
	}
	return filepath.Join(b.BinDir, name)
}

// available reports whether this node can build overlaybd layers, so a node
// refuses work it cannot do rather than failing the first create that needs it.
func (b *OverlaybdBuilder) available() error {
	for _, name := range []string{"overlaybd-create", "overlaybd-apply", "overlaybd-commit",
		// Needed on the create path, not just at conversion: every sandbox's
		// filesystem is grown to the requested size before its device is attached.
		"overlaybd-resize"} {
		if _, err := exec.LookPath(b.bin(name)); err != nil {
			return fmt.Errorf("image: overlaybd needs %s: %w", name, err)
		}
	}
	return nil
}

// sealedLayerPath is where a layer with this digest lives once built.
//
// Named by the OCI digest rather than by the image, because the whole point of
// keeping layers separate is that two images referencing the same layer reference
// the same file. A name derived from the image would defeat that.
func (b *OverlaybdBuilder) sealedLayerPath(digest string) string {
	return filepath.Join(b.LayerDir, sanitiseDigest(digest)+".obd")
}

// buildLayer converts one OCI layer tar into a sealed overlaybd layer and returns
// its path.
//
// tarPath must be the decompressed layer: overlaybd-apply reads a tar, not a
// tar.gz. The caller decompresses, because it is the one that knows what the
// registry sent.
//
// Already-built layers are returned as they are. Layers are immutable and named by
// digest, so an existing file is the same bytes by construction -- which is what
// makes a shared base cost its conversion once per node instead of once per image.
//
// parents are the already-sealed layers below this one, base first. A layer's tar is
// applied *over* them, because an OCI layer is a diff: it contains whiteouts and
// modified files that only mean anything relative to what is underneath. Applying one
// into an empty filesystem yields a layer holding just that diff, and a chain of such
// layers assembles into a rootfs missing everything the diffs did not restate.
//
// That failure is quiet in the worst way. Every layer seals, the chain opens
// (`open_lowers ... success`), the device appears and mounts -- and the guest finds an
// empty filesystem. Single-layer images work, which is why an end-to-end run against
// alpine passed while python:3.12-slim produced a sandbox with no /bin/sh.
func (b *OverlaybdBuilder) buildLayer(ctx context.Context, tarPath, digest string, vsizeGB int64, parents []string) (path string, err error) {
	final := b.sealedLayerPath(digest)
	if _, err := os.Stat(final); err == nil {
		return final, nil
	}

	for _, dir := range []string{b.LayerDir, b.WorkDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("image: create %s: %w", dir, err)
		}
	}

	stage, err := os.MkdirTemp(b.WorkDir, "obdlayer.*")
	if err != nil {
		return "", fmt.Errorf("image: stage overlaybd layer: %w", err)
	}
	defer os.RemoveAll(stage)

	dataPath := filepath.Join(stage, "data")
	indexPath := filepath.Join(stage, "index")

	// --mkfs only for the base layer, which is the one that has to contain a
	// filesystem. Formatting a layer that sits over others would write an empty
	// superblock on top of the filesystem they hold.
	createArgs := []string{dataPath, indexPath, fmt.Sprint(vsizeGB)}
	if len(parents) == 0 {
		createArgs = append([]string{"--mkfs"}, createArgs...)
	}
	if err := b.run(ctx, "overlaybd-create", createArgs...); err != nil {
		return "", err
	}

	// apply wants a config describing what it is writing into, not the raw layer
	// paths -- the same format the daemon reads. So a throwaway one is written with
	// this layer as the upper and its parents as the lowers, which is what makes the
	// tar's whiteouts and modified files resolve against the right base.
	//
	// The base layer's lowers are empty, and have to be: naming the layer as its own
	// lower asks overlaybd to open one file as both a read-only parent and a writable
	// target, which fails with only "failed to create image file".
	lowers := make([]obdLayer, 0, len(parents))
	for _, p := range parents {
		lowers = append(lowers, obdLayer{File: p})
	}
	applyCfg := filepath.Join(stage, "apply.json")
	if err := writeConfig(applyCfg, &obdConfig{
		Lowers: lowers,
		Upper:  obdUpper{Data: dataPath, Index: indexPath},
	}); err != nil {
		return "", err
	}

	args := []string{tarPath, applyCfg}
	if b.ServiceConfig != "" {
		args = append(args, "--service_config_path", b.ServiceConfig)
	}
	if err := b.run(ctx, "overlaybd-apply", args...); err != nil {
		return "", err
	}

	// -z compresses to zfile, which is what makes a remote layer cheap to
	// range-read: blocks are independently decompressable, so reading one file
	// does not mean fetching the whole layer. -t wraps the result in a tar so it
	// is a valid OCI blob and can be pushed to a registry as-is.
	sealed := filepath.Join(stage, "sealed")
	if err := b.run(ctx, "overlaybd-commit", "-z", "-t",
		dataPath, indexPath, sealed); err != nil {
		return "", err
	}

	// Published by rename so a concurrent build never observes a partial layer.
	// Two builds of the same digest race here and one loses; both produce
	// identical bytes, so the loser's rename simply wins or is overwritten by an
	// equivalent file.
	if err := os.Rename(sealed, final); err != nil {
		return "", fmt.Errorf("image: publish overlaybd layer: %w", err)
	}
	return final, nil
}

// createWritable makes the per-sandbox upper layer.
//
// -s makes it sparse, which is where the cheap fan-out comes from: the layer
// costs the blocks the sandbox writes, not its virtual size. Measured at 40 KiB
// for an idle sandbox against a 1.1 GB apparent size.
//
// No --mkfs: this layer sits over a lower that already holds the filesystem, and
// formatting it would hide that filesystem behind an empty one.
func (b *OverlaybdBuilder) createWritable(ctx context.Context, dir string, vsizeGB int64) (data, index string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("image: create writable dir: %w", err)
	}
	data = filepath.Join(dir, "writable.data")
	index = filepath.Join(dir, "writable.index")
	if err := b.run(ctx, "overlaybd-create", "-s",
		data, index, fmt.Sprint(vsizeGB)); err != nil {
		return "", "", err
	}
	return data, index, nil
}

// sealWritable turns a sandbox's writable layer into a sealed read-only one, which
// is what commit becomes on this backend: no ext4 is read out, the layer is simply
// sealed where it lies.
func (b *OverlaybdBuilder) sealWritable(ctx context.Context, data, index, dest string) error {
	return b.run(ctx, "overlaybd-commit", "-z", "-t", data, index, dest)
}

// resizeToGB grows the filesystem in an assembled chain to gb gigabytes.
//
// Takes the device config rather than a layer path because the filesystem spans the
// whole chain: it lives in the base layer, and growing it writes the new metadata
// through to the upper. That is what makes this safe to do per sandbox -- the shared
// read-only layers are not modified, only the writable one on top.
//
// Called before the device is attached. overlaybd-resize opens the chain itself, and a
// backstore already serving it would be a second writer to the same upper layer.
func (b *OverlaybdBuilder) resizeToGB(ctx context.Context, configPath string, gb int64) error {
	if gb <= 0 {
		return fmt.Errorf("image: resize needs a positive size, got %d", gb)
	}
	args := []string{"--config", configPath, "--size", fmt.Sprint(gb)}
	if b.ServiceConfig != "" {
		args = append(args, "--service_config_path", b.ServiceConfig)
	}
	return b.run(ctx, "overlaybd-resize", args...)
}

func (b *OverlaybdBuilder) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, b.bin(name), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image: %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
