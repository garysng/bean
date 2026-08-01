//go:build linux

package image

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests need loop devices and the device-mapper snapshot target, so they
// need root and a suitable kernel. They skip otherwise rather than failing, so
// `go test ./...` stays green on a developer machine and in unprivileged CI.
func newDevMapperProvider(t *testing.T) *DevMapperProvider {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("device-mapper tests need root")
	}
	dir := t.TempDir()
	p := NewDevMapperProvider(
		filepath.Join(dir, "sandboxes"),
		filepath.Join(dir, "images"),
		64,
	)
	if err := p.Available(); err != nil {
		t.Skipf("device-mapper unavailable: %v", err)
	}
	if err := os.MkdirAll(p.ImageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedExt4Image builds a real filesystem: the provider hands the device to a VM
// that mounts it, so a file of zeroes would not exercise the same thing.
func seedExt4Image(t *testing.T, p *DevMapperProvider, ref string, sizeMiB int64) {
	t.Helper()
	name, err := refToFilename(ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.ImageDir, name+".ext4")
	if err := createSparse(path, sizeMiB); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", path).CombinedOutput(); err != nil {
		t.Skipf("mkfs.ext4 unavailable: %v: %s", err, out)
	}
}

// mountAndRun mounts a device, runs fn against the mountpoint, and unmounts.
func mountAndRun(t *testing.T, device string, fn func(mnt string)) {
	t.Helper()
	mnt, err := os.MkdirTemp("", "beanmnt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(mnt)

	if out, err := exec.Command("mount", device, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount %s: %v: %s", device, err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			t.Errorf("umount %s: %v: %s", mnt, err, out)
		}
	}()
	fn(mnt)
}

// TestDevMapperClonesShareBaseWithoutCopying is the property that makes this
// provider worth its complexity: two sandboxes see the same base content, and
// neither one's writes reach the other or the base.
func TestDevMapperClonesShareBaseWithoutCopying(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "shared:1", 64)
	ctx := context.Background()

	first, err := p.Prepare(ctx, "sbx_dm_a", "shared:1", 64)
	if err != nil {
		t.Fatalf("prepare first: %v", err)
	}
	defer first.Release()
	second, err := p.Prepare(ctx, "sbx_dm_b", "shared:1", 64)
	if err != nil {
		t.Fatalf("prepare second: %v", err)
	}
	defer second.Release()

	mountAndRun(t, first.Device, func(mnt string) {
		if err := os.WriteFile(filepath.Join(mnt, "only-in-a.txt"), []byte("a"), 0o600); err != nil {
			t.Fatalf("write in first clone: %v", err)
		}
	})

	mountAndRun(t, second.Device, func(mnt string) {
		if _, err := os.Stat(filepath.Join(mnt, "only-in-a.txt")); !os.IsNotExist(err) {
			t.Error("a write in one clone is visible in another")
		}
		if err := os.WriteFile(filepath.Join(mnt, "only-in-b.txt"), []byte("b"), 0o600); err != nil {
			t.Fatalf("write in second clone: %v", err)
		}
	})

	// The first clone must not have picked up the second's write either.
	mountAndRun(t, first.Device, func(mnt string) {
		if _, err := os.Stat(filepath.Join(mnt, "only-in-b.txt")); !os.IsNotExist(err) {
			t.Error("clones are not isolated from each other")
		}
		if _, err := os.Stat(filepath.Join(mnt, "only-in-a.txt")); err != nil {
			t.Errorf("clone lost its own write: %v", err)
		}
	})
}

// TestDevMapperWriteCostsOnlyWhatChanged checks the economics. A copy-based
// provider would allocate the image size per sandbox; this must allocate only
// the changed blocks, which is what makes a large fan-out affordable.
func TestDevMapperWriteCostsOnlyWhatChanged(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "cheap:1", 64)

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_cost", "cheap:1", 64)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer rootfs.Release()

	mountAndRun(t, rootfs.Device, func(mnt string) {
		if err := os.WriteFile(filepath.Join(mnt, "small.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	// Well under the 64 MiB image: a copy would be at least that.
	if allocated := allocatedBytes(t, rootfs.Writable); allocated > 8<<20 {
		t.Errorf("copy-on-write store allocated %d bytes for a small write", allocated)
	}
}

// TestDevMapperBaseStaysPristine guards the invariant every clone depends on:
// the shared base must be opened read-only, or one sandbox could corrupt the
// image every other sandbox is reading.
func TestDevMapperBaseStaysPristine(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "pristine:1", 64)

	name, err := refToFilename("pristine:1")
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(p.ImageDir, name+".ext4")
	before, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_pristine", "pristine:1", 64)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	mountAndRun(t, rootfs.Device, func(mnt string) {
		if err := os.WriteFile(filepath.Join(mnt, "written.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err := rootfs.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	after, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("writing through a clone modified the shared base image")
	}
}

// TestDevMapperReleaseRemovesEverything matters because a leaked mapping keeps
// the base image busy and eventually exhausts the host's loop devices.
func TestDevMapperReleaseRemovesEverything(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "cleanup:1", 64)

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_clean", "cleanup:1", 64)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := rootfs.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if out, err := exec.Command("dmsetup", "ls").Output(); err == nil {
		if strings.Contains(string(out), dmName("sbx_dm_clean")) {
			t.Error("release left the device-mapper target behind")
		}
	}
	if _, err := os.Stat(rootfs.Writable); !os.IsNotExist(err) {
		t.Error("release left the copy-on-write store behind")
	}
	// The shared base's loop device must go once the last clone does.
	if out, err := exec.Command("losetup", "-a").Output(); err == nil {
		if strings.Contains(string(out), p.ImageDir) {
			t.Error("release left the base image attached to a loop device")
		}
	}
	// Idempotent: cleanup paths can reach Release more than once.
	if err := rootfs.Release(); err != nil {
		t.Errorf("second release = %v, want nil", err)
	}
}

// TestDevMapperSharesOneLoopDeviceAcrossClones verifies the base is attached
// once rather than per sandbox — the loop device count is a hard host limit.
func TestDevMapperSharesOneLoopDeviceAcrossClones(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "onedev:1", 64)
	ctx := context.Background()

	var made []*Rootfs
	for _, id := range []string{"sbx_dm_1", "sbx_dm_2", "sbx_dm_3"} {
		r, err := p.Prepare(ctx, id, "onedev:1", 64)
		if err != nil {
			t.Fatalf("prepare %s: %v", id, err)
		}
		made = append(made, r)
	}
	defer func() {
		for _, r := range made {
			_ = r.Release()
		}
	}()

	p.mu.Lock()
	bases := len(p.bases)
	var refs int
	for _, b := range p.bases {
		refs = b.refs
	}
	p.mu.Unlock()

	if bases != 1 {
		t.Errorf("attached %d base devices, want 1 shared", bases)
	}
	if refs != len(made) {
		t.Errorf("base refcount = %d, want %d", refs, len(made))
	}
}

func TestDevMapperReportsUncachedImage(t *testing.T) {
	p := newDevMapperProvider(t)
	_, err := p.Prepare(context.Background(), "sbx_dm_missing", "never-pulled:1", 64)
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("prepare = %v, want ErrNotCached", err)
	}
}

func TestDevMapperRequiresSandboxID(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "needsid:1", 64)
	if _, err := p.Prepare(context.Background(), "", "needsid:1", 64); err == nil {
		t.Error("prepare accepted an empty sandbox id")
	}
}
