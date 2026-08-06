package image

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The JSON key names are overlaybd's, not ours, and a mismatch is not an error it
// reports: the key is ignored and the device assembles missing that piece, which
// surfaces as a filesystem with the wrong contents. So the wire format is pinned.
func TestConfigMarshalsToOverlaybdKeyNames(t *testing.T) {
	cfg := &obdConfig{
		RepoBlobURL: "https://registry.example.com/v2/library/alpine/blobs",
		Lowers: []obdLayer{
			{File: "/layers/base.obd", Digest: "sha256:aaa", Size: 100},
			{Digest: "sha256:bbb", Size: 200},
		},
		Upper:      obdUpper{Data: "/sbx/writable.data", Index: "/sbx/writable.index"},
		ResultFile: "/sbx/result",
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	for _, key := range []string{
		`"repoBlobUrl"`, `"lowers"`, `"upper"`, `"resultFile"`,
		`"file"`, `"digest"`, `"size"`, `"index"`, `"data"`,
	} {
		if !strings.Contains(got, key) {
			t.Errorf("config JSON is missing %s: %s", key, got)
		}
	}

	// Absent optional fields must be omitted rather than emitted empty: overlaybd
	// treats an empty string as a value, so `"file": ""` is a layer claiming to be
	// local with no path instead of a remote one.
	if strings.Contains(got, `"targetFile"`) || strings.Contains(got, `"gzipIndex"`) {
		t.Errorf("unset optional fields should be omitted: %s", got)
	}
}

// Each of these is a config overlaybd would accept and then assemble incorrectly.
// The checks exist because the failure mode is a working device serving wrong
// bytes, not a refusal.
func TestConfigValidateRejectsSilentlyWrongChains(t *testing.T) {
	tests := []struct {
		name string
		cfg  obdConfig
		want string
	}{
		{
			name: "no lowers at all",
			cfg:  obdConfig{Upper: obdUpper{Data: "d", Index: "i"}},
			want: "no lower layers",
		},
		{
			name: "a layer naming neither a file nor a digest",
			cfg:  obdConfig{Lowers: []obdLayer{{Size: 10}}},
			want: "neither a file nor a digest",
		},
		{
			// The specific case that yields a chain silently short one layer:
			// overlaybd has a digest but nowhere to fetch it from.
			name: "remote layer with no repoBlobUrl anywhere",
			cfg:  obdConfig{Lowers: []obdLayer{{Digest: "sha256:aaa"}}},
			want: "no repoBlobUrl",
		},
		{
			// A cache dir is not a source: it starts empty and only holds blocks
			// already fetched, so a layer with a dir and no URL can never be read.
			name: "a cache dir is not a substitute for a repoBlobUrl",
			cfg: obdConfig{
				Lowers: []obdLayer{{Digest: "sha256:aaa", Size: 10, Dir: "/cache/aaa"}},
			},
			want: "no repoBlobUrl",
		},
		{
			// overlaybd range-reads a remote layer, so a zero length leaves it unable
			// to work out what to request.
			name: "remote layer with no size",
			cfg: obdConfig{
				RepoBlobURL: "http://s3/b/blobs",
				Lowers:      []obdLayer{{Digest: "sha256:aaa"}},
			},
			want: "no size",
		},
		{
			// A per-layer URL is not a source. overlaybd reads only the config-level
			// one -- __open_ro_remote uses conf.repoBlobUrl() while taking dir, digest
			// and size from the layer -- so a config like this passed validation and
			// then failed in the daemon with "empty repoBlobUrl for remote layer",
			// reaching the caller as a bare ENOENT on the enable write.
			name: "remote layer with only its own repoBlobUrl",
			cfg: obdConfig{
				Lowers: []obdLayer{
					{Digest: "sha256:aaa", Size: 10, RepoBlobURL: "http://s3/b/blobs"},
				},
			},
			want: "config sets no repoBlobUrl",
		},
		{
			// A lone data file is sparse mode to overlaybd, so accepting this
			// would quietly change the upper layer's format.
			name: "upper data without an index",
			cfg: obdConfig{
				Lowers: []obdLayer{{File: "/l/base.obd"}},
				Upper:  obdUpper{Data: "d"},
			},
			want: "data and index together",
		},
		{
			name: "upper index without data",
			cfg: obdConfig{
				Lowers: []obdLayer{{File: "/l/base.obd"}},
				Upper:  obdUpper{Index: "i"},
			},
			want: "data and index together",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if err == nil {
				t.Fatalf("validate() = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestConfigValidateAcceptsWorkableChains(t *testing.T) {
	tests := []struct {
		name string
		cfg  obdConfig
	}{
		{
			name: "local layers, no writable layer",
			cfg:  obdConfig{Lowers: []obdLayer{{File: "/l/base.obd"}}},
		},
		{
			name: "remote layers with a config-level repoBlobUrl",
			cfg: obdConfig{
				RepoBlobURL: "https://r/v2/x/blobs",
				Lowers: []obdLayer{
					{Digest: "sha256:aaa", Size: 10},
					{Digest: "sha256:bbb", Size: 20},
				},
				Upper: obdUpper{Data: "d", Index: "i"},
			},
		},
		{
			// The lazy-pull shape: read remotely, cache locally. Both set, because the
			// daemon prefers the cache and falls back to HTTP, which keeps the local
			// copy reclaimable. The URL is config-level because that is the only place
			// overlaybd reads it.
			name: "remote layer with a local cache dir",
			cfg: obdConfig{
				RepoBlobURL: "http://s3.example/bucket/blobs",
				Lowers: []obdLayer{{
					Digest: "sha256:aaa",
					Size:   36352,
					Dir:    "/var/lib/bean/images/layers/cache/sha256-aaa",
				}},
				Upper: obdUpper{Data: "d", Index: "i"},
			},
		},
		{
			// A mixed chain: one layer already on disk, one read remotely. This is
			// what a partially published image looks like, and it has to work rather
			// than being an all-or-nothing choice.
			name: "one local layer and one remote",
			cfg: obdConfig{
				RepoBlobURL: "http://s3.example/b/blobs",
				Lowers: []obdLayer{
					{File: "/l/base.obd", Digest: "sha256:aaa"},
					{Digest: "sha256:bbb", Size: 99},
				},
				Upper: obdUpper{Data: "d", Index: "i"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.validate(); err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
		})
	}
}

// overlaybd's compiled-in ceiling. Past it the daemon's behaviour is undefined
// rather than a clean refusal, so this is caught before it is handed over.
func TestConfigValidateRejectsTooManyLayers(t *testing.T) {
	cfg := obdConfig{Lowers: make([]obdLayer, maxLayers+1)}
	for i := range cfg.Lowers {
		cfg.Lowers[i] = obdLayer{File: "/l/x.obd"}
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "max 256") {
		t.Errorf("validate() = %v, want a max-layer error", err)
	}
}

// The daemon reads this file when the backstore is enabled. A partial read is not
// a parse error it reports -- it is a chain missing its tail, which assembles into
// a device serving the wrong bytes -- so the write has to be atomic.
func TestWriteConfigIsAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "overlaybd.json")

	want := &obdConfig{
		Lowers: []obdLayer{{File: "/l/base.obd", Digest: "sha256:aaa", Size: 42}},
		Upper:  obdUpper{Data: "/s/w.data", Index: "/s/w.index"},
	}
	if err := writeConfig(path, want); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got obdConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Lowers) != 1 || got.Lowers[0].File != "/l/base.obd" ||
		got.Upper.Data != "/s/w.data" || got.Upper.Index != "/s/w.index" {
		t.Errorf("round trip lost data: %+v", got)
	}

	// No staging file may survive, or the next write finds a stale one and the
	// directory accumulates them.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file outlived the write")
	}
}

// Layers are shared by digest across images, so the filename has to be derived
// from the digest and stay stable -- otherwise two images referencing one layer
// each convert their own copy and the sharing that justifies this backend is gone.
func TestSanitiseDigestIsStableAndPathSafe(t *testing.T) {
	got := sanitiseDigest("sha256:e2f5b9a1c3")
	if strings.ContainsAny(got, ":/") {
		t.Errorf("sanitiseDigest(%q) = %q, still contains a path separator", "sha256:...", got)
	}
	if got != sanitiseDigest("sha256:e2f5b9a1c3") {
		t.Error("sanitiseDigest is not stable across calls")
	}
	// Different digests must not collide, or one image would serve another's layer.
	if sanitiseDigest("sha256:aaa") == sanitiseDigest("sha256:bbb") {
		t.Error("distinct digests collided")
	}
}

// The serial and the loopback address both have to be unique per device on a host.
// A collision in the serial is the multipathd merge -- two devices treated as one
// LUN, serving each other's data with no error anywhere.
func TestShortHashDistinguishesSandboxes(t *testing.T) {
	seen := map[string]string{}
	for _, id := range []string{"sbx-1", "sbx-2", "sbx-10", "sbx_1", "a", "b"} {
		h := shortHash(id)
		if prev, ok := seen[h]; ok {
			t.Errorf("shortHash collided: %q and %q both hash to %q", prev, id, h)
		}
		seen[h] = id
		if h == "" {
			t.Errorf("shortHash(%q) is empty", id)
		}
	}
}

// Lazy pull only works on blobs that are already sealed overlaybd layers. This was
// implemented the other way round first -- handing overlaybd the digest of a standard
// OCI layer and expecting it to range-read a gzipped tar, which it cannot do. The
// distinction is a property of the image, not of the node's configuration, so it is
// worth a test rather than a comment.
func TestOnlySealedOverlaybdLayersCanBeReadRemotely(t *testing.T) {
	remote := []string{
		"application/vnd.containerd.overlaybd.layer.v1+tar",
		"application/vnd.containerd.overlaybd.layer.v1+tar+gzip",
		"application/vnd.oci.image.layer.v1.tar+overlaybd",
	}
	for _, mt := range remote {
		if !isOverlaybdLayer(mt) {
			t.Errorf("isOverlaybdLayer(%q) = false, want true", mt)
		}
	}

	local := []string{
		// The ordinary cases: any image from a registry. These have no block index
		// to seek into, so they must be converted locally.
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
		"application/vnd.oci.image.layer.v1.tar+gzip",
		"application/vnd.oci.image.layer.v1.tar",
		"application/vnd.oci.image.layer.v1.tar+zstd",
		// turbo-OCI is an overlaybd format but carries only indexes over the
		// original layers, so reading one means resolving its target too.
		"application/vnd.containerd.overlaybd.turbo.v1+json",
		"",
	}
	for _, mt := range local {
		if isOverlaybdLayer(mt) {
			t.Errorf("isOverlaybdLayer(%q) = true, want false", mt)
		}
	}
}

// Measured on hardware: the kernel builds a TCMU WWID from the hex-digit characters
// of the unit serial and discards the rest, so "bean-aaa" became
// naa.6001405beaaaa000... That makes a non-hex serial actively dangerous rather than
// merely untidy -- "bean-sbx-alpha" and "bean-probe-2" both reduce to "beabaa", and
// two devices with one WWID are merged by multipathd into a device that serves
// whichever it picked.
func TestHexSerialKeepsOnlyWhatTheKernelKeeps(t *testing.T) {
	tests := []struct{ serial, want string }{
		// The measurements this was derived from.
		{"bean-aaa", "beaaaa"},
		{"bean-bbb", "beabbb"},
		{"bean-diag2", "beada2"},
		// The collision that motivates rejecting non-hex serials.
		{"bean-sbx-alpha", "beabaa"},
		{"bean-probe-2", "beabe2"},
		// Already hex: unchanged, which is what the provider must produce.
		{"beaf02", "beaf02"},
		{"0123456789abcdef", "0123456789abcdef"},
		// Case is normalised, since the kernel reports the WWID in lower case.
		{"BEAF02", "beaf02"},
	}
	for _, tc := range tests {
		if got := hexSerial(tc.serial); got != tc.want {
			t.Errorf("hexSerial(%q) = %q, want %q", tc.serial, got, tc.want)
		}
	}
}

// The provider's serials must survive the kernel's filter unchanged, or the value it
// registers is not the value the code believes it set.
func TestDeviceSerialIsHexOnlyAndDistinct(t *testing.T) {
	// Ids deliberately including the shapes that collide once non-hex characters
	// are dropped: without hashing, "sbx-alpha" and "s-b-x-a-l-p-h-a" are the same
	// serial.
	ids := []string{
		"sbx-1", "sbx-2", "sbx-alpha", "s-b-x-a-l-p-h-a",
		"sbx_1", "SBX-1", "eval-run-0731-task-django-12345",
	}
	seen := map[string]string{}
	for _, id := range ids {
		s := deviceSerial(id)

		// The round trip through the kernel's filter has to be the identity, or the
		// WWID encodes something other than what was written.
		if hexSerial(s) != s {
			t.Errorf("deviceSerial(%q) = %q, which the kernel would reduce to %q",
				id, s, hexSerial(s))
		}
		if prev, ok := seen[s]; ok {
			t.Errorf("deviceSerial collided: %q and %q both give %q -- multipathd "+
				"would merge these devices", prev, id, s)
		}
		seen[s] = id
	}
}

// Stability matters because a device is looked up by the serial derived from its
// sandbox id: a serial that changed between attach and lookup would find nothing, or
// worse, find another sandbox's device.
func TestDeviceSerialIsStable(t *testing.T) {
	if a, b := deviceSerial("sbx-42"), deviceSerial("sbx-42"); a != b {
		t.Errorf("deviceSerial is not stable: %q then %q", a, b)
	}
}

// overlaybd reads repoBlobUrl from the config root only: __open_ro_remote uses
// conf.repoBlobUrl() while taking dir, digest and size from the layer. A per-layer key
// is silently ignored, so this asserts on where the URL lands in the JSON rather than
// merely that it round-trips through the struct.
//
// The cost of getting it wrong is not a clear error. The daemon logs "empty repoBlobUrl
// for remote layer" and the caller sees ENOENT on the enable write, naming neither the
// layer nor the key.
func TestConfigSerialisesTheBlobURLAtTheRootOnly(t *testing.T) {
	cfg := obdConfig{
		RepoBlobURL: "http://s3.example/bucket/blobs",
		Lowers: []obdLayer{{
			Digest:      "sha256:aaa",
			Size:        36352,
			Dir:         "/cache/aaa",
			RepoBlobURL: "http://ignored.example/blobs",
		}},
		Upper: obdUpper{Data: "d", Index: "i"},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		RepoBlobURL string `json:"repoBlobUrl"`
		Lowers      []map[string]any
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.RepoBlobURL != "http://s3.example/bucket/blobs" {
		t.Errorf("root repoBlobUrl = %q", got.RepoBlobURL)
	}
	if len(got.Lowers) != 1 {
		t.Fatalf("got %d lowers", len(got.Lowers))
	}
	// Present in the layer, it would read as a source that overlaybd never consults,
	// which is the belief this whole check exists to keep out of the config.
	if _, ok := got.Lowers[0]["repoBlobUrl"]; ok {
		t.Error("layer serialised a repoBlobUrl; overlaybd does not read one")
	}
	if got.Lowers[0]["dir"] != "/cache/aaa" {
		t.Errorf("layer dir = %v, want it kept per-layer", got.Lowers[0]["dir"])
	}
}
