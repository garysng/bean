//go:build linux

package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeBlob writes content into the layout's blob tree and returns its descriptor.
func writeBlob(t *testing.T, layoutDir, mediaType string, content []byte) Descriptor {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := fmt.Sprintf("sha256:%x", sum)
	dir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest[len("sha256:"):]), content, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(content))}
}

// gzipLayer builds a gzipped tar layer from a set of path->content entries.
func gzipLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// buildLayout writes a minimal single-platform OCI image layout and returns its dir.
// indexWrapsImage controls whether index.json points straight at the image manifest
// (the common single build) or wraps it in a nested index (a multi-platform build).
func buildLayout(t *testing.T, indexWrapsImage bool) string {
	t.Helper()
	layoutDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0o600); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}

	// Two layers: the second overwrites a file from the first, proving order.
	l1 := writeBlob(t, layoutDir, "application/vnd.oci.image.layer.v1.tar+gzip",
		gzipLayer(t, map[string]string{"etc/base": "one", "bin/tool": "v1"}))
	l2 := writeBlob(t, layoutDir, "application/vnd.oci.image.layer.v1.tar+gzip",
		gzipLayer(t, map[string]string{"bin/tool": "v2"}))

	configJSON, err := json.Marshal(map[string]any{
		"config": map[string]any{
			"Env":        []string{"PATH=/usr/bin", "FOO=bar"},
			"Entrypoint": []string{"/bin/tool"},
			"Cmd":        []string{"--serve"},
			"WorkingDir": "/app",
			"User":       "1000",
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	cfgDesc := writeBlob(t, layoutDir, mediaTypeOCIConfig, configJSON)

	manifestJSON, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaTypeOCIManifest,
		"config":        cfgDesc,
		"layers":        []Descriptor{l1, l2},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manDesc := writeBlob(t, layoutDir, mediaTypeOCIManifest, manifestJSON)

	var indexEntry Descriptor = manDesc
	if indexWrapsImage {
		nestedIndex, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     mediaTypeOCIIndex,
			"manifests": []map[string]any{{
				"mediaType": mediaTypeOCIManifest,
				"digest":    manDesc.Digest,
				"size":      manDesc.Size,
				"platform":  map[string]string{"os": "linux", "architecture": "amd64"},
			}},
		})
		if err != nil {
			t.Fatalf("marshal nested index: %v", err)
		}
		indexEntry = writeBlob(t, layoutDir, mediaTypeOCIIndex, nestedIndex)
	}

	indexJSON, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType": indexEntry.MediaType,
			"digest":    indexEntry.Digest,
			"size":      indexEntry.Size,
		}},
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), indexJSON, 0o600); err != nil {
		t.Fatalf("write index.json: %v", err)
	}
	return layoutDir
}

// readTarFiles reads a plain (uncompressed) tar into a path->content map.
func readTarFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		out[hdr.Name] = string(b)
	}
	return out
}

func TestParseOCILayoutRecoversConfigAndFlattens(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		name := "direct"
		if wrap {
			name = "nested-index"
		}
		t.Run(name, func(t *testing.T) {
			layoutDir := buildLayout(t, wrap)
			res, err := parseOCILayout(layoutDir, t.TempDir())
			if err != nil {
				t.Fatalf("parseOCILayout: %v", err)
			}
			if res.Config == nil {
				t.Fatal("expected a config, got nil")
			}
			if got, want := res.Config.WorkingDir, "/app"; got != want {
				t.Errorf("WorkingDir = %q, want %q", got, want)
			}
			if len(res.Config.Entrypoint) != 1 || res.Config.Entrypoint[0] != "/bin/tool" {
				t.Errorf("Entrypoint = %v, want [/bin/tool]", res.Config.Entrypoint)
			}
			if len(res.Config.Env) != 2 || res.Config.Env[1] != "FOO=bar" {
				t.Errorf("Env = %v, want PATH + FOO=bar", res.Config.Env)
			}

			files := readTarFiles(t, res.RootfsTar)
			if got := files["etc/base"]; got != "one" {
				t.Errorf("etc/base = %q, want one", got)
			}
			// The second layer must win: flattening applies layers base-first.
			if got := files["bin/tool"]; got != "v2" {
				t.Errorf("bin/tool = %q, want v2 (later layer overwrites)", got)
			}
		})
	}
}

func TestParseOCILayoutSkipsAttestationManifests(t *testing.T) {
	layoutDir := buildLayout(t, false)
	// Append an attestation-style manifest entry to index.json; the parser must skip
	// it and still find the image manifest.
	raw, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx map[string]any
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	manifests := idx["manifests"].([]any)
	manifests = append(manifests, map[string]any{
		"mediaType": "application/vnd.in-toto+json",
		"digest":    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"size":      10,
	})
	idx["manifests"] = manifests
	patched, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), patched, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	res, err := parseOCILayout(layoutDir, t.TempDir())
	if err != nil {
		t.Fatalf("parseOCILayout with attestation present: %v", err)
	}
	if res.Config == nil || res.Config.WorkingDir != "/app" {
		t.Errorf("expected image config recovered past the attestation entry")
	}
}
