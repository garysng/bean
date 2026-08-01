package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFileProvider(t *testing.T) *FileProvider {
	t.Helper()
	dir := t.TempDir()
	p := &FileProvider{
		BaseDir:        filepath.Join(dir, "sandboxes"),
		ImageDir:       filepath.Join(dir, "images"),
		DefaultSizeMiB: 64,
	}
	if err := os.MkdirAll(p.ImageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedImage writes a base image of the given size, standing in for what the
// image service produces from an OCI reference.
func seedImage(t *testing.T, p *FileProvider, ref string, contents string) {
	t.Helper()
	name, err := refToFilename(ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.ImageDir, name+".ext4")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareGivesEachSandboxAnIndependentRootfs(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "alpine:3.20", "base-image-bytes")
	ctx := context.Background()

	first, err := p.Prepare(ctx, "sbx_a", "alpine:3.20", 8)
	if err != nil {
		t.Fatalf("prepare first: %v", err)
	}
	defer first.Release()
	second, err := p.Prepare(ctx, "sbx_b", "alpine:3.20", 8)
	if err != nil {
		t.Fatalf("prepare second: %v", err)
	}
	defer second.Release()

	if first.Device == second.Device {
		t.Fatal("two sandboxes share one device")
	}
	if first.ReadOnly {
		t.Error("sandbox rootfs must be writable")
	}

	// Writing through one must not be visible in the other: this is the
	// property that makes a snapshot fan-out safe.
	if err := os.WriteFile(first.Device, []byte("changed-by-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(second.Device)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(other), "changed-by-a") {
		t.Error("write to one sandbox leaked into another")
	}
}

func TestPrepareSizesTheRootfs(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "img", "small")
	ctx := context.Background()

	rootfs, err := p.Prepare(ctx, "sbx_sized", "img", 16)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer rootfs.Release()

	info, err := os.Stat(rootfs.Device)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 16<<20 {
		t.Errorf("rootfs size = %d, want %d", info.Size(), 16<<20)
	}
	// The file must stay sparse: a 16 MiB rootfs holding 5 bytes should not
	// occupy 16 MiB, or a node's disk accounting is meaningless.
	if blocks := allocatedBytes(t, rootfs.Device); blocks > 1<<20 {
		t.Errorf("rootfs allocated %d bytes, expected a sparse file", blocks)
	}
}

func TestPrepareUsesDefaultSizeWhenUnbounded(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "img", "x")

	rootfs, err := p.Prepare(context.Background(), "sbx_default", "img", 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer rootfs.Release()

	info, err := os.Stat(rootfs.Device)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != p.DefaultSizeMiB<<20 {
		t.Errorf("size = %d MiB, want the provider default %d MiB",
			info.Size()>>20, p.DefaultSizeMiB)
	}
}

// TestPrepareRejectsUndersizedRequest guards against silently truncating an
// image, which would produce a corrupt filesystem the guest fails to mount.
func TestPrepareRejectsUndersizedRequest(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "big", strings.Repeat("x", 3<<20))

	if _, err := p.Prepare(context.Background(), "sbx_small", "big", 1); err == nil {
		t.Error("prepare accepted a size smaller than the base image")
	}
	// The failed attempt must leave nothing behind.
	if entries, err := os.ReadDir(p.BaseDir); err == nil && len(entries) != 0 {
		t.Errorf("failed prepare left %d directories behind", len(entries))
	}
}

func TestPrepareReportsUncachedImage(t *testing.T) {
	p := newFileProvider(t)

	_, err := p.Prepare(context.Background(), "sbx_missing", "never-pulled:latest", 8)
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("prepare = %v, want ErrNotCached", err)
	}
	if err := p.Prewarm(context.Background(), "never-pulled:latest"); !errors.Is(err, ErrNotCached) {
		t.Errorf("prewarm = %v, want ErrNotCached", err)
	}
}

func TestPrepareRequiresSandboxID(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "img", "x")
	if _, err := p.Prepare(context.Background(), "", "img", 8); err == nil {
		t.Error("prepare accepted an empty sandbox id")
	}
}

func TestPrewarmSucceedsForCachedImage(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "cached:1", "bytes")
	if err := p.Prewarm(context.Background(), "cached:1"); err != nil {
		t.Errorf("prewarm cached image = %v", err)
	}
}

// TestReleaseIsIdempotent matters because cleanup runs on both the success and
// failure paths, sometimes both.
func TestReleaseIsIdempotent(t *testing.T) {
	p := newFileProvider(t)
	seedImage(t, p, "img", "x")

	rootfs, err := p.Prepare(context.Background(), "sbx_release", "img", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootfs.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if _, err := os.Stat(rootfs.Device); !os.IsNotExist(err) {
		t.Error("release did not remove the rootfs")
	}
	if err := rootfs.Release(); err != nil {
		t.Errorf("second release = %v, want nil", err)
	}
	// A zero-value Rootfs must also be safe to release.
	if err := (&Rootfs{}).Release(); err != nil {
		t.Errorf("release of an empty Rootfs = %v", err)
	}
	var nilRootfs *Rootfs
	if err := nilRootfs.Release(); err != nil {
		t.Errorf("release of a nil Rootfs = %v", err)
	}
}

// TestRefToFilenameIsSafeAndDistinct checks the mapping cannot escape the
// image directory and does not collide across refs that differ only in
// punctuation.
func TestRefToFilenameIsSafeAndDistinct(t *testing.T) {
	if _, err := refToFilename(""); err == nil {
		t.Error("empty ref accepted")
	}

	seen := map[string]string{}
	refs := []string{
		"alpine:3.20",
		"alpine:3_20",
		"docker.io/library/alpine:3.20",
		"docker.io/library/alpine@sha256:abc",
		"../../etc/passwd",
		"a/b",
		"a_b",
	}
	for _, ref := range refs {
		name, err := refToFilename(ref)
		if err != nil {
			t.Fatalf("refToFilename(%q): %v", ref, err)
		}
		if strings.ContainsAny(name, "/\\.") {
			t.Errorf("refToFilename(%q) = %q, contains a path separator", ref, name)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("refs %q and %q both map to %q", prev, ref, name)
		}
		seen[name] = ref
	}
}
