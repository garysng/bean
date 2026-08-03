//go:build linux

package image

import (
	"context"
	"errors"
	"io"
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

	first, err := p.Prepare(ctx, "sbx_dm_a", "shared:1", PrepareOptions{SizeMiB: 64})
	if err != nil {
		t.Fatalf("prepare first: %v", err)
	}
	defer first.Release()
	second, err := p.Prepare(ctx, "sbx_dm_b", "shared:1", PrepareOptions{SizeMiB: 64})
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

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_cost", "cheap:1", PrepareOptions{SizeMiB: 64})
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

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_pristine", "pristine:1", PrepareOptions{SizeMiB: 64})
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

	rootfs, err := p.Prepare(context.Background(), "sbx_dm_clean", "cleanup:1", PrepareOptions{SizeMiB: 64})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := rootfs.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if out, err := exec.Command("dmsetup", "ls").Output(); err == nil {
		if strings.Contains(string(out), DMName("sbx_dm_clean")) {
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
		r, err := p.Prepare(ctx, id, "onedev:1", PrepareOptions{SizeMiB: 64})
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
	_, err := p.Prepare(context.Background(), "sbx_dm_missing", "never-pulled:1", PrepareOptions{SizeMiB: 64})
	if !errors.Is(err, ErrNotCached) {
		t.Errorf("prepare = %v, want ErrNotCached", err)
	}
}

func TestDevMapperRequiresSandboxID(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "needsid:1", 64)
	if _, err := p.Prepare(context.Background(), "", "needsid:1", PrepareOptions{SizeMiB: 64}); err == nil {
		t.Error("prepare accepted an empty sandbox id")
	}
}

// TestDevMapperSeedIsVisibleThroughDevice is the property a restore depends on
// and the one that was missing when a restored sandbox silently lost its files.
//
// A device-mapper snapshot reads its exception table at activation and never
// re-reads it, so a copy-on-write store written after the device exists is
// ignored: the device serves the base image while the file holds data. Reading
// the base through the mapping is the only way to see that, which is why this
// mounts the device instead of inspecting cow.img — every file-level assertion
// passes against the broken ordering.
func TestDevMapperSeedIsVisibleThroughDevice(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "seeded:1", 64)
	ctx := context.Background()

	// A first sandbox writes a file, which lands in its copy-on-write store.
	// That store is what a checkpoint carries, so it stands in for one here.
	source, err := p.Prepare(ctx, "sbx_seed_src", "seeded:1", PrepareOptions{SizeMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	mountAndRun(t, source.Device, func(mnt string) {
		if err := os.WriteFile(filepath.Join(mnt, "marker"), []byte("survives"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	captured := filepath.Join(t.TempDir(), "captured.img")
	if err := copyFileContents(source.Writable, captured); err != nil {
		t.Fatal(err)
	}
	if err := source.Release(); err != nil {
		t.Fatal(err)
	}

	// A second sandbox is prepared with that store as its seed, which is what a
	// restore does.
	restored, err := p.Prepare(ctx, "sbx_seed_dst", "seeded:1", PrepareOptions{
		SizeMiB:      64,
		SeedWritable: func(dest string) error { return copyFileContents(captured, dest) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Release()

	mountAndRun(t, restored.Device, func(mnt string) {
		content, err := os.ReadFile(filepath.Join(mnt, "marker"))
		if err != nil {
			t.Fatalf("seeded file is not readable through the device: %v", err)
		}
		if string(content) != "survives" {
			t.Errorf("seeded file reads %q through the device, want %q — the "+
				"exception table does not describe the seeded store",
				content, "survives")
		}
	})
}

// TestDevMapperSeedFailureLeavesNothing keeps a failed restore from being
// mistaken for a fresh sandbox: the seed runs mid-Prepare, after the store
// exists, so its error has to unwind the same way the later steps do.
func TestDevMapperSeedFailureLeavesNothing(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "seedfail:1", 64)

	_, err := p.Prepare(context.Background(), "sbx_seed_fail", "seedfail:1", PrepareOptions{
		SizeMiB:      64,
		SeedWritable: func(string) error { return errors.New("bundle truncated") },
	})
	if err == nil {
		t.Fatal("Prepare succeeded despite a failed seed")
	}
	if !strings.Contains(err.Error(), "bundle truncated") {
		t.Errorf("error %q does not name the seed failure", err)
	}
	if _, err := os.Stat(filepath.Join(p.BaseDir, "sbx_seed_fail")); !os.IsNotExist(err) {
		t.Errorf("sandbox directory survived a failed seed: %v", err)
	}
	if out, _ := exec.Command("dmsetup", "info", DMName("sbx_seed_fail")).CombinedOutput(); !strings.Contains(string(out), "does not exist") {
		t.Errorf("mapping survived a failed seed: %s", out)
	}
}

// copyFileContents copies a file's bytes, preserving holes so a sparse store
// stays sparse.
func copyFileContents(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_CREATE so this serves both a fresh capture file and a store the provider
	// already created at its provisioned size.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Truncate(st.Size()); err != nil {
		return err
	}
	return out.Sync()
}

// loopsFor counts the loop devices backed by a path, which is what leaks when a
// restarted node re-attaches a base image it is already holding.
func loopsFor(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("losetup", "--noheadings", "--output", "NAME",
		"--associated", path).Output()
	if err != nil {
		t.Skipf("losetup --associated unavailable: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestDevMapperAdoptsAnExistingBaseLoop is the restart case. The reference count
// that decides when to detach a shared base lives in this process's memory, so a
// new provider starts it at zero — and without looking for an existing device it
// attaches the same file again, holding the old one for the life of the host.
//
// Measured on a real host before the fix: one leaked device per node restart,
// each pinning a base image that could then never be reclaimed.
func TestDevMapperAdoptsAnExistingBaseLoop(t *testing.T) {
	p := newDevMapperProvider(t)
	seedExt4Image(t, p, "adopted:1", 64)
	ctx := context.Background()

	// Derived with the same helper the provider uses, so the test does not
	// encode the naming scheme a second time.
	name, err := refToFilename("adopted:1")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(p.ImageDir, name+imageSuffix)
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base image absent at %s: %v", base, err)
	}

	first, err := p.Prepare(ctx, "sbx_adopt_a", "adopted:1", PrepareOptions{SizeMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	afterFirst := loopsFor(t, base)
	if afterFirst != 1 {
		t.Fatalf("one sandbox produced %d loop devices for the base, want 1", afterFirst)
	}

	// A second provider over the same directories stands in for a restarted
	// node: same files on disk, no in-memory reference count.
	restarted := NewDevMapperProvider(p.BaseDir, p.ImageDir, 64)
	second, err := restarted.Prepare(ctx, "sbx_adopt_b", "adopted:1", PrepareOptions{SizeMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	if got := loopsFor(t, base); got != 1 {
		t.Errorf("after a simulated restart the base has %d loop devices, want 1: "+
			"each restart leaks one and pins the image forever", got)
	}
}
