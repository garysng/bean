//go:build linux

package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These exercise the real overlaybd path: real binaries, real configfs, real block
// devices. They skip unless the host can support it, so the suite still runs on a
// developer machine -- but the claims this package makes about hardware behaviour are
// only actually checked here.
//
// Requires root (configfs writes) and a running overlaybd-tcmu.
func requireOverlaybd(t *testing.T) *OverlaybdBuilder {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("overlaybd needs root for configfs")
	}
	if err := tcmuAvailable(); err != nil {
		t.Skip(err.Error())
	}
	b := NewOverlaybdBuilder("/opt/overlaybd/bin", t.TempDir(), t.TempDir())
	if err := b.available(); err != nil {
		t.Skip(err.Error())
	}
	return b
}

// A layer has to be sealed before it can be a lower. Handed an unsealed one the
// daemon fails with "trailer magic ... or sealedness doesn't match" and leaves the
// backstore DEACTIVATED, which reaches the caller only as ENOENT on the enable write
// -- so this checks the pipeline produces something the daemon will actually open.
func TestBuildLayerProducesASealedLayer(t *testing.T) {
	b := requireOverlaybd(t)
	ctx := context.Background()

	// A tar with one identifiable file, so a later mount can prove the contents
	// arrived rather than merely that a device appeared.
	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}

	path, err := b.buildLayer(ctx, tarPath, "sha256:test0001", 2)
	if err != nil {
		t.Fatalf("buildLayer: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("sealed layer missing: %v", err)
	}
	if info.Size() == 0 {
		t.Error("sealed layer is empty")
	}

	// Naming by digest is what makes layers shared across images rather than
	// converted per image, so a second call must not redo the work.
	again, err := b.buildLayer(ctx, tarPath, "sha256:test0001", 2)
	if err != nil {
		t.Fatalf("second buildLayer: %v", err)
	}
	if again != path {
		t.Errorf("layer path changed: %q then %q", path, again)
	}
}

// The full path: build a layer, attach it with a writable layer on top, and read the
// contents through the resulting block device. This is the claim that matters -- a
// device appearing is not the same as the chain being assembled correctly.
func TestAttachServesTheLayerContents(t *testing.T) {
	b := requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	lower, err := b.buildLayer(ctx, tarPath, "sha256:test0002", 2)
	if err != nil {
		t.Fatalf("buildLayer: %v", err)
	}

	dir := t.TempDir()
	data, index, err := b.createWritable(ctx, dir, 2)
	if err != nil {
		t.Fatalf("createWritable: %v", err)
	}

	cfgPath := filepath.Join(dir, "device.json")
	cfg := &obdConfig{
		Lowers: []obdLayer{{File: lower}},
		Upper:  obdUpper{Data: data, Index: index},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := writeConfig(cfgPath, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	serial := deviceSerial("hwtest-attach")
	dev, err := attachTCMU("beanhwtest", cfgPath, serial, 2<<30)
	if err != nil {
		t.Fatalf("attachTCMU: %v", err)
	}
	t.Cleanup(func() {
		if err := dev.detach(); err != nil {
			t.Errorf("detach leaked configfs objects: %v", err)
		}
	})

	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-o", "ro", dev.Device, mnt).CombinedOutput(); err != nil {
		// "already mounted or mount point busy" here is the multipathd merge: the
		// device was absorbed into an mpath device and cannot be mounted directly.
		t.Fatalf("mount %s: %v: %s", dev.Device, err, out)
	}
	defer exec.Command("umount", mnt).Run()

	if _, err := os.Stat(filepath.Join(mnt, "beanmarker")); err != nil {
		entries, _ := os.ReadDir(mnt)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the layer's file is not in the mounted device; found %v", names)
	}
}

// The serial is what keeps multipathd from merging two devices and serving one
// sandbox another's data. Two attached devices must present different WWIDs.
func TestTwoDevicesGetDistinctWWIDs(t *testing.T) {
	b := requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	lower, err := b.buildLayer(ctx, tarPath, "sha256:test0003", 2)
	if err != nil {
		t.Fatalf("buildLayer: %v", err)
	}

	devices := map[string]string{}
	for _, id := range []string{"hwtest-a", "hwtest-b"} {
		dir := t.TempDir()
		data, index, err := b.createWritable(ctx, dir, 2)
		if err != nil {
			t.Fatalf("createWritable: %v", err)
		}
		cfgPath := filepath.Join(dir, "device.json")
		if err := writeConfig(cfgPath, &obdConfig{
			Lowers: []obdLayer{{File: lower}},
			Upper:  obdUpper{Data: data, Index: index},
		}); err != nil {
			t.Fatalf("writeConfig: %v", err)
		}

		dev, err := attachTCMU("beanhw"+id, cfgPath, deviceSerial(id), 2<<30)
		if err != nil {
			t.Fatalf("attachTCMU %s: %v", id, err)
		}
		t.Cleanup(func() { _ = dev.detach() })

		if prev, ok := devices[dev.Device]; ok {
			t.Fatalf("%s and %s resolved to the same device %s -- the serial did not "+
				"distinguish them, which is the multipathd merge", prev, id, dev.Device)
		}
		devices[dev.Device] = id
	}
	if len(devices) != 2 {
		t.Errorf("expected two distinct devices, got %v", devices)
	}
}

// A serial the kernel would silently shorten must be refused rather than accepted,
// since two serials that reduce to the same hex digits present as one LUN.
func TestAttachRefusesANonHexSerial(t *testing.T) {
	requireOverlaybd(t)
	_, err := attachTCMU("beanhwbad", "/nonexistent.json", "bean-sbx-alpha", 1<<30)
	if err == nil {
		t.Fatal("attachTCMU accepted a non-hex serial")
	}
	if !strings.Contains(err.Error(), "hex digits only") {
		t.Errorf("error does not explain the serial constraint: %v", err)
	}
}

// writeTestTar makes a one-file tar, using the tar binary so the archive is exactly
// what a registry layer looks like to overlaybd-apply.
func writeTestTar(dest string) error {
	dir, err := os.MkdirTemp("", "beantar")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "beanmarker"), []byte("bean\n"), 0o644); err != nil {
		return err
	}
	return exec.Command("tar", "-cf", dest, "-C", dir, "beanmarker").Run()
}
