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
// A layer is either local (File) or remote (Digest plus the config's
// RepoBlobURL). Both are spelled here because a node legitimately holds some
// layers and streams others -- a base that has been pulled once, plus a task
// layer being read on demand.
type obdLayer struct {
	// File is a local path to the layer blob. Empty means fetch it by digest.
	File string `json:"file,omitempty"`
	// Digest identifies the blob in the registry, e.g. "sha256:...".
	Digest string `json:"digest,omitempty"`
	// Size is the blob's length. overlaybd wants it up front so it can range-read
	// without a HEAD first.
	Size int64 `json:"size,omitempty"`
	// RepoBlobURL overrides the config-level one for this layer, for a chain whose
	// layers come from different repositories.
	RepoBlobURL string `json:"repoBlobUrl,omitempty"`
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
		// yields a chain silently short one layer.
		if l.File == "" && l.RepoBlobURL == "" && c.RepoBlobURL == "" {
			return fmt.Errorf("image: layer %d is remote but no repoBlobUrl is set", i)
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
