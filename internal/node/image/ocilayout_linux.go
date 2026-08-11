//go:build linux

package image

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// An OCI image layout is the directory BuildKit's `type=oci` exporter writes: an
// `oci-layout` marker, an `index.json` naming the top-level manifest(s), and a
// `blobs/<algo>/<hex>` tree holding every manifest, config and layer by digest.
// bean parses it to recover two things a flat `type=tar` export dropped: the
// image config (ENV/ENTRYPOINT/CMD/WORKDIR/USER the Dockerfile declared) and the
// layers, which are flattened back into a single rootfs tar.
//
// The layers are flattened rather than preserved as a chain on purpose. A built
// image is sealed as one content-addressed overlaybd layer keyed by the sha256 of
// its sealed bytes (PublishBuiltRootfs), and BuildKit re-gzips layers on export, so
// their digests would not match a registry's for the same content -- preserving the
// chain buys no dedup against pulled images while forcing the seal path to fetch and
// stack blobs it has no registry for. Flattening keeps the build's one-layer shape
// and recovers the config, which is the part that was actually lost.

// ociLayoutResult is what parsing an OCI layout yields: the recovered image config
// (nil when the image records none) and the path to a single flattened rootfs tar.
type ociLayoutResult struct {
	Config    *Config
	RootfsTar string
}

// mediaTypeOCIConfig is the OCI image config blob's media type. A manifest whose
// config uses a different type is not a runnable image -- an attestation manifest
// carries `application/vnd.in-toto+json` config -- and is skipped when selecting the
// image manifest from the index.
const mediaTypeOCIConfig = "application/vnd.oci.image.config.v1+json"

// parseOCILayout reads the OCI image layout rooted at layoutDir, flattens its layers
// into a rootfs tar written under workDir, and returns that path together with the
// image config. The caller owns the returned tar and removes it.
func parseOCILayout(layoutDir, workDir string) (ociLayoutResult, error) {
	manifest, err := ociLayoutManifest(layoutDir)
	if err != nil {
		return ociLayoutResult{}, err
	}

	cfg, err := ociLayoutConfig(layoutDir, manifest.Config)
	if err != nil {
		return ociLayoutResult{}, err
	}

	rootfsTar, err := flattenOCILayers(layoutDir, workDir, manifest.Layers)
	if err != nil {
		return ociLayoutResult{}, err
	}
	return ociLayoutResult{Config: cfg, RootfsTar: rootfsTar}, nil
}

// ociLayoutManifest reads index.json and resolves it to the single image manifest,
// following a multi-platform index to linux/amd64 and skipping non-image manifests
// (attestation/provenance) a modern BuildKit adds alongside the image.
func ociLayoutManifest(layoutDir string) (*Manifest, error) {
	indexBytes, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("image: read oci layout index: %w", err)
	}

	var index struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Platform  *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return nil, fmt.Errorf("image: parse oci layout index: %w", err)
	}
	if len(index.Manifests) == 0 {
		return nil, errors.New("image: oci layout index has no manifests")
	}

	// The index entry may itself be a nested index (a multi-platform build), or the
	// image manifest directly (the common single-platform build). Resolve to the
	// linux/amd64 image manifest either way.
	digest := ""
	for _, m := range index.Manifests {
		switch m.MediaType {
		case mediaTypeOCIIndex, mediaTypeManifestList:
			nested, err := os.ReadFile(blobPath(layoutDir, m.Digest))
			if err != nil {
				return nil, fmt.Errorf("image: read nested index: %w", err)
			}
			target, err := pickPlatform(nested)
			if err != nil {
				return nil, err
			}
			digest = target
		case mediaTypeOCIManifest, mediaTypeManifestV2:
			// A single-platform build names the image manifest here. When the entry
			// carries a platform, honour linux/amd64; entries without one (the usual
			// single build) are taken as-is.
			if m.Platform != nil && !(m.Platform.OS == "linux" && m.Platform.Architecture == "amd64") {
				continue
			}
			if digest == "" {
				digest = m.Digest
			}
		default:
			// Attestation and other non-image manifests: skip.
		}
	}
	if digest == "" {
		return nil, errors.New("image: no linux/amd64 image manifest in oci layout")
	}

	manifestBytes, err := os.ReadFile(blobPath(layoutDir, digest))
	if err != nil {
		return nil, fmt.Errorf("image: read oci layout manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("image: parse oci layout manifest: %w", err)
	}
	if m.Config.MediaType != "" && m.Config.MediaType != mediaTypeOCIConfig &&
		!strings.HasSuffix(m.Config.MediaType, "image.config.v1+json") {
		return nil, fmt.Errorf("image: oci layout manifest is not a runnable image (config %s)", m.Config.MediaType)
	}
	m.Digest = digest
	return &m, nil
}

// ociLayoutConfig reads and parses the image config blob. A descriptor with no
// digest means the manifest names no config, which is not something BuildKit
// produces but is handled as "no config recorded" rather than an error.
func ociLayoutConfig(layoutDir string, desc Descriptor) (*Config, error) {
	if desc.Digest == "" {
		return nil, nil
	}
	blob, err := os.ReadFile(blobPath(layoutDir, desc.Digest))
	if err != nil {
		return nil, fmt.Errorf("image: read oci layout config: %w", err)
	}
	// The image config is `{..., "config": {Env, Entrypoint, Cmd, WorkingDir, User}}`
	// -- the same wrapper FetchConfig unmarshals from a registry, so the runtime sees
	// a built image's config identically to a pulled one's.
	var wrapper struct {
		Config Config `json:"config"`
	}
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return nil, fmt.Errorf("image: parse oci layout config: %w", err)
	}
	return &wrapper.Config, nil
}

// flattenOCILayers applies each layer in order into one uncompressed rootfs tar,
// decompressing gzip layers on the way. The result is the flat filesystem tar the
// seal and write paths already consume, so recovering config does not change how a
// built image is assembled downstream.
//
// Layers are applied base-first, honouring OCI whiteouts, by materialising them onto
// a scratch directory and re-taring it -- the same extractTar the pull and build
// paths use, so whiteout and containment handling match.
func flattenOCILayers(layoutDir, workDir string, layers []Descriptor) (string, error) {
	scratch, err := os.MkdirTemp(workDir, "ocilayers.*")
	if err != nil {
		return "", fmt.Errorf("image: create layer scratch: %w", err)
	}
	defer os.RemoveAll(scratch)

	for _, layer := range layers {
		if err := applyOCILayoutLayer(layoutDir, layer, scratch); err != nil {
			return "", err
		}
	}

	outTar, err := os.CreateTemp(workDir, "rootfs.*.tar")
	if err != nil {
		return "", fmt.Errorf("image: create flattened tar: %w", err)
	}
	defer outTar.Close()
	if err := tarDirectoryPlain(scratch, outTar); err != nil {
		os.Remove(outTar.Name())
		return "", fmt.Errorf("image: write flattened tar: %w", err)
	}
	return outTar.Name(), nil
}

// applyOCILayoutLayer unpacks one layer blob over the scratch root.
func applyOCILayoutLayer(layoutDir string, layer Descriptor, root string) error {
	f, err := os.Open(blobPath(layoutDir, layer.Digest))
	if err != nil {
		return fmt.Errorf("image: open oci layout layer: %w", err)
	}
	defer f.Close()

	var src io.Reader = f
	// Most layers are gzipped; the media type says so, but registries and exporters
	// are loose about it, so the magic bytes decide -- mirroring applyLayer.
	if strings.Contains(layer.MediaType, "gzip") || strings.HasSuffix(layer.MediaType, "tar+gzip") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("image: open gzip oci layer: %w", err)
		}
		defer zr.Close()
		src = zr
	}
	return extractTar(tar.NewReader(src), root)
}

// tarDirectoryPlain writes dir as an uncompressed tar. The build path wants an
// uncompressed rootfs (sizeForTar reads its length as content size and extractTar
// reads a plain tar), so this does not gzip -- unlike the runtime's tarDirectory,
// which is for a different consumer and in a package this one cannot import.
//
// Regular files, directories and symlinks are recorded; symlinks as links rather
// than followed, so the containment the extractor enforces on read is matched on
// write. Whiteouts have already been resolved by extractTar as each layer was applied
// onto the scratch root, so the flattened tar carries only surviving files.
func tarDirectoryPlain(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// blobPath maps an OCI content digest ("sha256:abcd...") to its file in the layout's
// blobs tree ("<layoutDir>/blobs/sha256/abcd...").
func blobPath(layoutDir, digest string) string {
	algo, hex, found := strings.Cut(digest, ":")
	if !found {
		// A malformed digest names no file; the read that follows fails with a clear
		// path rather than this guessing at a layout.
		algo, hex = "sha256", digest
	}
	return filepath.Join(layoutDir, "blobs", algo, hex)
}
