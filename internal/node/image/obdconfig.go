package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// overlaybd is driven by a per-device JSON config rather than by command-line
// arguments: the tcmu daemon reads the file named in a backstore's control
// attribute and assembles the device from what it finds there. So the config file
// _is_ the interface, and this file owns producing one overlaybd will accept.
//
// The field names are overlaybd's, not ours, which is why they are spelled in
// camelCase here and why this type is separate from anything else in the package.
// A name that does not match is not a validation error -- overlaybd ignores the
// key and assembles a device missing a layer, which surfaces much later as a
// filesystem that mounts and has the wrong contents.

// obdConfig is one device's layer chain.
//
// A device is the whole chain: read-only lowers in order, plus one writable upper
// belonging to a single sandbox. Fanning out N sandboxes from one image means N
// configs that share every lower and differ only in upper.
type obdConfig struct {
	// RepoBlobURL is where a layer's blob is fetched from when the layer itself
	// names no source. This is what makes lazy pull work: the daemon range-reads
	// the registry instead of the node holding the layer.
	RepoBlobURL string `json:"repoBlobUrl,omitempty"`
	// Lowers are the read-only layers, base first. Order is the caller's
	// contract and cannot be recovered from the data.
	Lowers []obdLayer `json:"lowers"`
	// Upper is this sandbox's writable layer.
	Upper obdUpper `json:"upper"`
	// ResultFile is where overlaybd writes the outcome of assembling the device.
	// Worth setting even though nothing reads it on the happy path: when a device
	// fails to appear this file holds the reason, and the alternative is guessing
	// from an empty configfs.
	ResultFile string `json:"resultFile,omitempty"`
	// AccelerationLayer and RecordTracePath drive overlaybd's access-trace
	// prefetching. Off here: recording a trace is only useful once there is a
	// trace to replay, and that is a separate piece of work from making the
	// device appear at all.
	AccelerationLayer bool   `json:"accelerationLayer,omitempty"`
	RecordTracePath   string `json:"recordTracePath,omitempty"`
}

// obdLayer is one read-only layer in the chain.
//
// A layer is either local (File) or remote (Digest, resolved against the config's
// top-level RepoBlobURL). Both are spelled here because a node legitimately holds some
// layers and streams others -- a base that has been pulled once, plus a task layer
// being read on demand.
//
// The consequence of the URL being config-level is that **every remote layer in one
// chain must come from the same prefix**. That holds here because remote layers are
// either all from bean's object store or all from one image's registry repository, but
// it is a constraint of overlaybd's config rather than a choice.
type obdLayer struct {
	// File is a local path to the layer blob. Empty means fetch it by digest.
	File string `json:"file,omitempty"`
	// Digest identifies the blob in the registry, e.g. "sha256:...".
	Digest string `json:"digest,omitempty"`
	// Size is the blob's length. overlaybd wants it up front so it can range-read
	// without a HEAD first.
	Size int64 `json:"size,omitempty"`
	// RepoBlobURL records where this layer is fetched from, and is *not* serialised:
	// the field exists so the resolver can carry a per-layer answer through to the
	// point where the config is built, which then hoists it to the config root.
	//
	// overlaybd reads only the top-level `repoBlobUrl` -- `__open_ro_remote` uses
	// `conf.repoBlobUrl()` while taking dir, digest and size from the layer. A
	// per-layer key is silently ignored, and the failure is
	// "empty repoBlobUrl for remote layer" followed by ENOENT on the enable write,
	// which names neither the layer nor the key that was wrong.
	RepoBlobURL string `json:"-"`
	// VsizeGB is the virtual size the layer's filesystem was formatted to, and like
	// RepoBlobURL it is *not* serialised: overlaybd reads the size from the layer
	// itself, so this exists only to carry the figure from the resolver to the caller
	// that has to size the device over it.
	//
	// Set on the first layer only, which is the one that carries a filesystem. Zero
	// means unknown, and a caller must treat that as "no constraint" rather than as
	// zero -- a remotely read chain does not always have a manifest to hand.
	//
	// It exists because the device and the filesystem were sized independently: the
	// writable layer from the caller's diskMiB, the base from vsizeForImage's 2 GB
	// floor. A create with diskMiB=512 then produced a 1 GB device over a 2 GB
	// filesystem, and the guest kernel refused it -- "bad geometry: block count
	// 524288 exceeds size of device (262144 blocks)".
	VsizeGB int64 `json:"-"`
	// Dir is a local cache directory for a remotely read layer.
	//
	// Set alongside a remote reference rather than instead of one: overlaybd serves
	// from the cache when the blocks are there and falls back to HTTP when they are
	// not, so the local copy stays reclaimable. Using File instead would make the
	// local copy load-bearing, with no fallback if it were evicted.
	//
	// Without it the daemon logs "local dir of layer N didn't set, skip background
	// anyway" and every read goes to the network.
	Dir string `json:"dir,omitempty"`
	// TargetFile and TargetDigest name the underlying OCI layer for a turboOCI
	// layer, whose blob is an index over the original tar rather than a
	// self-contained image of it.
	TargetFile   string `json:"targetFile,omitempty"`
	TargetDigest string `json:"targetDigest,omitempty"`
	// GzipIndex is the index that makes a gzipped turboOCI target seekable.
	GzipIndex string `json:"gzipIndex,omitempty"`
}

// obdUpper is the writable layer, which is per sandbox and never shared.
//
// Data and Index are a pair and must be set together: the index maps virtual
// offsets to positions in the data file, so one without the other describes
// nothing. overlaybd's own validation accepts a lone Data only in sparse mode.
type obdUpper struct {
	Index string `json:"index,omitempty"`
	Data  string `json:"data,omitempty"`
}

// writeConfig serialises a config to path atomically.
//
// Atomically because the tcmu daemon reads this file when the backstore is
// enabled, and a partially written one is not a parse error it reports -- it is a
// chain missing its tail, which assembles into a device serving the wrong bytes.
func writeConfig(path string, cfg *obdConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("image: marshal overlaybd config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("image: create config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("image: write overlaybd config: %w", err)
	}
	return os.Rename(tmp, path)
}

// validate rejects a config overlaybd would accept and then assemble wrongly.
//
// It exists because overlaybd's failure mode for a malformed chain is a device
// that appears and serves incorrect data rather than an error. Every check here
// is a case where being permissive costs a silent wrong answer.
//
// Applies to configs describing a *device*. The throwaway config handed to
// overlaybd-apply while building a layer legitimately has no lowers, and is not
// validated -- see buildLayer.
func (c *obdConfig) validate() error {
	if len(c.Lowers) == 0 {
		return errors.New("image: overlaybd config has no lower layers")
	}
	// overlaybd's own limit. Past it the daemon's behaviour is undefined rather
	// than a clean refusal.
	if len(c.Lowers) > maxLayers {
		return fmt.Errorf("image: overlaybd config has %d layers, max %d",
			len(c.Lowers), maxLayers)
	}
	for i, l := range c.Lowers {
		if l.File == "" && l.Digest == "" {
			return fmt.Errorf("image: layer %d names neither a file nor a digest", i)
		}
		// A digest without somewhere to fetch it from is the specific case that
		// yields a chain silently short one layer. A cache `dir` does not count as a
		// source: it starts empty and is only where fetched blocks are kept, so a
		// layer with a dir and no URL has nowhere to fetch from at all.
		//
		// Only the config-level URL is accepted. A layer's own RepoBlobURL is not
		// serialised and overlaybd would not read it if it were, so treating it as a
		// source here is what let a config pass validation and then fail in the daemon
		// with "empty repoBlobUrl for remote layer".
		if l.File == "" && c.RepoBlobURL == "" {
			return fmt.Errorf("image: layer %d is remote but the config sets no repoBlobUrl", i)
		}
		// A remote layer needs its size: overlaybd range-reads it, and a zero length
		// leaves it unable to work out what to ask for.
		if l.File == "" && l.Size <= 0 {
			return fmt.Errorf("image: remote layer %d has no size", i)
		}
	}
	// Enforced rather than papered over: overlaybd treats a lone data file as
	// sparse mode, so accepting a half-set pair would quietly change the upper
	// layer's format instead of failing.
	if (c.Upper.Data == "") != (c.Upper.Index == "") {
		return errors.New("image: overlaybd upper needs data and index together")
	}
	return nil
}

// maxLayers is overlaybd's compiled-in ceiling on chain length.
const maxLayers = 256

// Media types for blobs that are already sealed overlaybd layers, as published by
// accelerated-container-image's converter.
const (
	mediaTypeOverlaybdLayer    = "application/vnd.containerd.overlaybd.layer.v1+tar"
	mediaTypeOverlaybdLayerGz  = "application/vnd.containerd.overlaybd.layer.v1+tar+gzip"
	mediaTypeOverlaybdTurboV1  = "application/vnd.containerd.overlaybd.turbo.v1+json"
	mediaTypeOverlaybdBlockTar = "application/vnd.oci.image.layer.v1.tar+overlaybd"
)

// isOverlaybdLayer reports whether a blob can be read directly from a registry by
// the overlaybd daemon.
//
// This is the distinction that decides whether lazy pull is possible at all. A
// sealed overlaybd layer is an LSMT structure whose blocks are individually
// addressable, so the daemon can range-read it over HTTP. A standard OCI layer is a
// gzipped tar: there is no block index to seek into, and the whole thing has to be
// fetched and converted before it can back a device.
//
// So "lazy pull" is a property of the image, not of the node's configuration. An
// ordinary image from Docker Hub cannot be lazily pulled no matter what this node is
// told to do -- it first has to be converted and pushed in overlaybd form.
//
// turbo-OCI is excluded deliberately even though it is an overlaybd format: those
// blobs carry only indexes over the original OCI layers, so reading one means
// resolving its target layer as well, which this provider does not implement.
func isOverlaybdLayer(mediaType string) bool {
	switch mediaType {
	case mediaTypeOverlaybdLayer, mediaTypeOverlaybdLayerGz, mediaTypeOverlaybdBlockTar:
		return true
	case mediaTypeOverlaybdTurboV1:
		// Named rather than falling through to the default, so a turbo image is
		// refused by a case that says why instead of by omission.
		return false
	}
	return false
}

// sanitiseDigest turns "sha256:abc..." into a usable filename.
//
// The colon is the only character an OCI digest carries that a path cannot, so
// this is a replacement rather than a general encoding -- and unlike
// refToFilename it stays reversible, because a digest is the identity a layer is
// looked up by. Layers are shared across images by that name, so it also has to be
// stable: a filename derived any other way would have two images each convert
// their own copy of a layer they share.
func sanitiseDigest(digest string) string {
	return strings.ReplaceAll(digest, ":", "-")
}

// shortHash gives a stable hex suffix for a name, for the parts of the configfs
// tree that need a unique-but-derived identifier.
//
// FNV-1a rather than a cryptographic hash: the requirement is that two sandboxes
// on one host do not collide, not that the value is unforgeable.
func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return strconv.FormatUint(uint64(h), 16)
}

// hexSerial is the part of a serial the kernel puts in a TCMU device's WWID: its
// hex-digit characters, in order, lowercased.
//
// Measured on hardware rather than read from documentation: serial "bean-aaa"
// produced naa.6001405beaaaa000..., "bean-diag2" produced naa.6001405beada2000...
//
// This is why serials have to be hex-only. "bean-sbx-alpha" and "bean-probe-2" both
// reduce to "beabaa", so two sandboxes with those ids present identical WWIDs -- and
// identical WWIDs is precisely the multipathd merge that serves one sandbox another's
// data. A serial that looks unique and is not is worse than an obviously shared one,
// because nothing reports it.
func hexSerial(serial string) string {
	var b strings.Builder
	for i := 0; i < len(serial); i++ {
		c := serial[i] | 0x20 // lowercase
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// deviceSerial is the SCSI unit serial for a sandbox's device.
//
// Hex only, and that is a hard requirement rather than tidiness: the kernel builds
// the device's WWID from the hex digits of the serial and discards everything else,
// so a serial containing other characters is silently truncated -- and two serials
// that reduce to the same digits present as one LUN, which is the multipathd merge
// that hands a sandbox another's data with no error anywhere.
//
// A hash rather than the sandbox id, because ids are not hex: "sbx-alpha" would
// reduce to "aa" and collide with half the fleet. Sixteen hex digits from two
// rounds keeps the space large enough that a collision on one host is not a
// practical concern.
func deviceSerial(sandboxID string) string {
	a := shortHash(sandboxID)
	b := shortHash("bean-serial-" + sandboxID)
	return fmt.Sprintf("%08s%08s", a, b)
}
