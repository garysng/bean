//go:build linux

package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
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

	path, err := b.buildLayer(ctx, tarPath, "sha256:test0001", 2, nil)
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
	again, err := b.buildLayer(ctx, tarPath, "sha256:test0001", 2, nil)
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
	lower, err := b.buildLayer(ctx, tarPath, "sha256:test0002", 2, nil)
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

// An OCI layer is a diff, so a layer's tar has to be applied over the layers below it.
// Building each into an empty filesystem instead produces a chain that seals, opens
// (`open_lowers ... success`), attaches and mounts -- holding nothing. Single-layer
// images are unaffected, which is how this survived an end-to-end run against alpine
// while python:3.12-slim gave a sandbox with no /bin/sh at all.
//
// So the assertion is that a file from the *lower* layer is still there after an upper
// layer has been applied.
func TestAChainPreservesLowerLayerFiles(t *testing.T) {
	b := requireOverlaybd(t)
	ctx := context.Background()

	// Layer 1 writes two files; layer 2 modifies one and adds a third. A correct chain
	// has all three, with the modification winning.
	base := filepath.Join(t.TempDir(), "base.tar")
	if err := writeTar(base, map[string]string{"frombase": "base\n", "overwrite": "old\n"}); err != nil {
		t.Fatalf("build base tar: %v", err)
	}
	upper := filepath.Join(t.TempDir(), "upper.tar")
	if err := writeTar(upper, map[string]string{"overwrite": "new\n", "fromupper": "upper\n"}); err != nil {
		t.Fatalf("build upper tar: %v", err)
	}

	lower1, err := b.buildLayer(ctx, base, "sha256:chain0001", 2, nil)
	if err != nil {
		t.Fatalf("buildLayer base: %v", err)
	}
	lower2, err := b.buildLayer(ctx, upper, "sha256:chain0002", 1, []string{lower1})
	if err != nil {
		t.Fatalf("buildLayer upper: %v", err)
	}

	dir := t.TempDir()
	data, index, err := b.createWritable(ctx, dir, 2)
	if err != nil {
		t.Fatalf("createWritable: %v", err)
	}
	cfgPath := filepath.Join(dir, "device.json")
	if err := writeConfig(cfgPath, &obdConfig{
		Lowers: []obdLayer{{File: lower1}, {File: lower2}},
		Upper:  obdUpper{Data: data, Index: index},
	}); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	dev, err := attachTCMU("beanhwchain", cfgPath, deviceSerial("hwtest-chain"), 2<<30)
	if err != nil {
		t.Fatalf("attachTCMU: %v", err)
	}
	t.Cleanup(func() { _ = dev.detach() })

	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-o", "ro", dev.Device, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount %s: %v: %s", dev.Device, err, out)
	}
	defer exec.Command("umount", mnt).Run()

	for _, tc := range []struct{ file, want string }{
		{"frombase", "base\n"}, // the one that regressed
		{"overwrite", "new\n"},
		{"fromupper", "upper\n"},
	} {
		got, err := os.ReadFile(filepath.Join(mnt, tc.file))
		if err != nil {
			entries, _ := os.ReadDir(mnt)
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s: %v (device holds %v)", tc.file, err, names)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.file, got, tc.want)
		}
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
	lower, err := b.buildLayer(ctx, tarPath, "sha256:test0003", 2, nil)
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
	return writeTar(dest, map[string]string{"beanmarker": "bean\n"})
}

// writeTar builds a tar containing the given files.
func writeTar(dest string, files map[string]string) error {
	dir, err := os.MkdirTemp("", "beantar")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	names := make([]string, 0, len(files))
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
		names = append(names, name)
	}
	return exec.Command("tar", append([]string{"-cf", dest, "-C", dir}, names...)...).Run()
}

// TestConcurrentCreatesConvertALayerOnce checks the dedup is wired into the path
// callers actually take, not merely that layerFlight works in isolation. The unit
// tests in obdflight_test.go pass whether or not materialiseLayer calls it.
//
// Counted at the registry, because that is where duplicated work is observable and
// expensive: the rename in buildLayer already made a concurrent conversion produce
// correct bytes, so the only symptom of missing dedup is the same blob fetched and
// converted N times.
func TestConcurrentCreatesConvertALayerOnce(t *testing.T) {
	builder := requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	var blobFetches atomic.Int32
	digest := "sha256:" + strings.Repeat("c1", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", mediaTypeManifestV2)
			fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":%q,`+
				`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg","size":2},`+
				`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				mediaTypeManifestV2, digest, len(layerGz))
		case strings.Contains(r.URL.Path, "/blobs/sha256:cfg"):
			w.Write([]byte(`{}`))
		case strings.Contains(r.URL.Path, "/blobs/"):
			blobFetches.Add(1)
			// Slow enough that every goroutine is inside lowersFor before the
			// first conversion finishes. Without that the leader could complete
			// and let the others find the sealed file, which would pass for the
			// wrong reason.
			time.Sleep(300 * time.Millisecond)
			w.Write(layerGz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	reg := NewRegistry(nil)
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	p := NewOverlaybdProvider(t.TempDir(), builder.LayerDir, t.TempDir(), reg, builder, 512)

	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, errs[i] = p.lowersFor(ctx, strings.TrimPrefix(srv.URL, "http://")+"/test/img:latest", false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := blobFetches.Load(); got != 1 {
		t.Errorf("layer blob fetched %d times, want 1: conversions are not deduplicated", got)
	}
}

// gzipFile returns the gzip-compressed contents of path, which is the form a
// registry serves a layer in.
func gzipFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// obdTestRegistry serves a single-layer image and counts blob fetches, so a test can
// tell conversion from a lookup that avoided it.
type obdTestRegistry struct {
	digest  string
	layerGz []byte
	fetches atomic.Int32
}

func (r *obdTestRegistry) serve(t *testing.T) (string, *Registry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", mediaTypeManifestV2)
			fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":%q,`+
				`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg","size":2},`+
				`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				mediaTypeManifestV2, r.digest, len(r.layerGz))
		case strings.Contains(req.URL.Path, "/blobs/sha256:cfg"):
			w.Write([]byte(`{}`))
		case strings.Contains(req.URL.Path, "/blobs/"):
			r.fetches.Add(1)
			w.Write(r.layerGz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	reg := NewRegistry(nil)
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	return strings.TrimPrefix(srv.URL, "http://") + "/test/img:latest", reg
}

// countingBlobs records what was published and what was looked up, which is how the
// separation between create and prewarm is observable at all: both produce a working
// layer chain, and only the store's call log distinguishes them.
type countingBlobs struct {
	mu    sync.Mutex
	blobs map[string]int64
	puts  int
}

func newCountingBlobs() *countingBlobs {
	return &countingBlobs{blobs: map[string]int64{}}
}

func (c *countingBlobs) Stat(_ context.Context, digest string) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size, ok := c.blobs[digest]
	return size, ok, nil
}

func (c *countingBlobs) Put(_ context.Context, digest string, size int64, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	c.blobs[digest] = int64(len(body))
	_ = size
	return nil
}

func (c *countingBlobs) BlobURL() string { return "http://blobs.example/bean/blobs" }

// The readability probe is a deployment check, not a layer-resolution one, so it is
// satisfied here rather than exercised.
func (c *countingBlobs) CheckReadable(context.Context) error { return nil }

// obdLazyProvider builds a provider whose layer directory is empty, so what it does
// with an image is decided entirely by the lookup levels.
func obdLazyProvider(t *testing.T, reg *Registry, blobs BlobStore) *OverlaybdProvider {
	t.Helper()
	builder := NewOverlaybdBuilder("/opt/overlaybd/bin", t.TempDir(), t.TempDir())
	p := NewOverlaybdProvider(t.TempDir(), builder.LayerDir, t.TempDir(), reg, builder, 512)
	p.LazyPull = true
	p.Blobs = blobs
	return p
}

// A create is not the place for an upload: the bytes are already on this node, and the
// only beneficiary is a later create that may never happen. Prewarm exists for that.
func TestCreateDoesNotPublishButPrewarmDoes(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("d1", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)

	blobs := newCountingBlobs()
	p := obdLazyProvider(t, reg, blobs)
	if _, _, _, err := p.lowersFor(ctx, ref, false); err != nil {
		t.Fatalf("lowersFor: %v", err)
	}
	if blobs.puts != 0 {
		t.Errorf("a create published %d layers; publication belongs to prewarm", blobs.puts)
	}

	// Prewarm on a node that already converted has nothing to convert but does have
	// something to publish, so the upload is not conditional on the conversion.
	if err := p.Prewarm(ctx, ref); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	if blobs.puts != 1 {
		t.Errorf("prewarm published %d layers, want 1", blobs.puts)
	}
}

// The point of publishing: a node that has never seen the image references the
// published layer instead of converting it. Asserted on the registry fetch count,
// because a conversion and a remote reference both yield a usable chain.
func TestCreateReadsAPublishedLayerInsteadOfConverting(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	digest := "sha256:" + strings.Repeat("d2", 32)
	fake := &obdTestRegistry{digest: digest, layerGz: layerGz}
	ref, reg := fake.serve(t)
	blobs := newCountingBlobs()

	// First node converts and publishes.
	first := obdLazyProvider(t, reg, blobs)
	if err := first.Prewarm(ctx, ref); err != nil {
		t.Fatalf("prewarm on the first node: %v", err)
	}
	if blobs.puts != 1 {
		t.Fatalf("published %d layers, want 1", blobs.puts)
	}
	afterFirst := fake.fetches.Load()

	// Second node has its own empty layer directory.
	second := obdLazyProvider(t, reg, blobs)
	lowers, _, _, err := second.lowersFor(ctx, ref, false)
	if err != nil {
		t.Fatalf("lowersFor on the second node: %v", err)
	}
	if got := fake.fetches.Load(); got != afterFirst {
		t.Errorf("second node fetched the layer blob %d more times; it converted instead of "+
			"reading the published copy", got-afterFirst)
	}
	if len(lowers) != 1 {
		t.Fatalf("got %d lowers, want 1", len(lowers))
	}
	if lowers[0].File != "" {
		t.Errorf("layer referenced as a local file %q, want a remote reference", lowers[0].File)
	}
	if lowers[0].RepoBlobURL != blobs.BlobURL() {
		t.Errorf("repoBlobUrl = %q, want %q", lowers[0].RepoBlobURL, blobs.BlobURL())
	}
	// The size must be the published blob's, not the manifest's figure for the
	// original OCI layer: a remote layer is range-read against it.
	if want := blobs.blobs[digest]; lowers[0].Size != want {
		t.Errorf("size = %d, want the sealed blob's %d", lowers[0].Size, want)
	}
}

// A local layer must stay local. Referencing bytes this node already holds as a remote
// blob would make a read that cannot fail depend on the store being up.
func TestLocalLayerIsPreferredOverThePublishedCopy(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("d3", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	blobs := newCountingBlobs()

	p := obdLazyProvider(t, reg, blobs)
	if err := p.Prewarm(ctx, ref); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	// The same node now has both a local file and a published blob.
	lowers, _, _, err := p.lowersFor(ctx, ref, false)
	if err != nil {
		t.Fatalf("lowersFor: %v", err)
	}
	if lowers[0].File == "" {
		t.Error("a layer this node holds was referenced remotely")
	}
	if lowers[0].RepoBlobURL != "" {
		t.Errorf("local layer carries repoBlobUrl %q", lowers[0].RepoBlobURL)
	}
}

// A prewarm must produce a local chain. Conversion applies a layer's tar over its
// parents as files, so if a prewarm took a remote reference for an early layer it would
// have no local parent for the next one -- which is exactly how a prewarm failed
// against its own earlier publication, on a node where some layers were already in the
// store.
func TestPrewarmConvertsLocallyEvenWhenLayersArePublished(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	base := filepath.Join(t.TempDir(), "base.tar")
	if err := writeTar(base, map[string]string{"base": "base\n"}); err != nil {
		t.Fatalf("base tar: %v", err)
	}
	top := filepath.Join(t.TempDir(), "top.tar")
	if err := writeTar(top, map[string]string{"top": "top\n"}); err != nil {
		t.Fatalf("top tar: %v", err)
	}
	baseGz, err := gzipFile(base)
	if err != nil {
		t.Fatalf("gzip base: %v", err)
	}
	topGz, err := gzipFile(top)
	if err != nil {
		t.Fatalf("gzip top: %v", err)
	}

	baseDigest := "sha256:" + strings.Repeat("e1", 32)
	topDigest := "sha256:" + strings.Repeat("e2", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", mediaTypeManifestV2)
			fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":%q,`+
				`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg","size":2},`+
				`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d},`+
				`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				mediaTypeManifestV2, baseDigest, len(baseGz), topDigest, len(topGz))
		case strings.Contains(req.URL.Path, "/blobs/sha256:cfg"):
			w.Write([]byte(`{}`))
		case strings.Contains(req.URL.Path, "/blobs/"+baseDigest):
			w.Write(baseGz)
		case strings.Contains(req.URL.Path, "/blobs/"+topDigest):
			w.Write(topGz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	reg := NewRegistry(nil)
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/img:latest"

	// The base layer is already in the store, the top one is not: the state a node is
	// in when it prewarms an image whose base it shares with something published.
	blobs := newCountingBlobs()
	seeder := obdLazyProvider(t, reg, blobs)
	if _, _, err := seeder.walk(ctx, ref, resolveOpts{publish: true}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	blobs.mu.Lock()
	delete(blobs.blobs, topDigest)
	blobs.mu.Unlock()

	fresh := obdLazyProvider(t, reg, blobs)
	if err := fresh.Prewarm(ctx, ref); err != nil {
		t.Fatalf("prewarm with a published base: %v", err)
	}
	if _, ok, _ := blobs.Stat(ctx, topDigest); !ok {
		t.Error("the top layer was not published")
	}
}

// A create against a partly published image must still start. The chain cannot mix a
// remote parent with a converted child, so it converts the whole thing locally --
// slower than the remote read, and better than refusing a create that can succeed.
func TestCreateFallsBackToLocalConversionWhenOnlySomeLayersArePublished(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	base := filepath.Join(t.TempDir(), "base.tar")
	if err := writeTar(base, map[string]string{"base": "base\n"}); err != nil {
		t.Fatalf("base tar: %v", err)
	}
	top := filepath.Join(t.TempDir(), "top.tar")
	if err := writeTar(top, map[string]string{"top": "top\n"}); err != nil {
		t.Fatalf("top tar: %v", err)
	}
	baseGz, err := gzipFile(base)
	if err != nil {
		t.Fatalf("gzip base: %v", err)
	}
	topGz, err := gzipFile(top)
	if err != nil {
		t.Fatalf("gzip top: %v", err)
	}

	baseDigest := "sha256:" + strings.Repeat("f1", 32)
	topDigest := "sha256:" + strings.Repeat("f2", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", mediaTypeManifestV2)
			fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":%q,`+
				`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg","size":2},`+
				`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d},`+
				`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				mediaTypeManifestV2, baseDigest, len(baseGz), topDigest, len(topGz))
		case strings.Contains(req.URL.Path, "/blobs/sha256:cfg"):
			w.Write([]byte(`{}`))
		case strings.Contains(req.URL.Path, "/blobs/"+baseDigest):
			w.Write(baseGz)
		case strings.Contains(req.URL.Path, "/blobs/"+topDigest):
			w.Write(topGz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	reg := NewRegistry(nil)
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/img:latest"

	blobs := newCountingBlobs()
	seeder := obdLazyProvider(t, reg, blobs)
	if _, _, err := seeder.walk(ctx, ref, resolveOpts{publish: true}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	blobs.mu.Lock()
	delete(blobs.blobs, topDigest)
	blobs.mu.Unlock()

	fresh := obdLazyProvider(t, reg, blobs)
	lowers, _, _, err := fresh.lowersFor(ctx, ref, false)
	if err != nil {
		t.Fatalf("create against a partly published image: %v", err)
	}
	if len(lowers) != 2 {
		t.Fatalf("got %d lowers, want 2", len(lowers))
	}
	// Every layer local, including the one that was published: a mixed chain is what
	// cannot be built.
	for i, l := range lowers {
		if l.File == "" {
			t.Errorf("layer %d resolved remotely; the fallback should be entirely local", i)
		}
	}
}

// The availability property: a node that has the image must start it with the registry
// unreachable. dm-snapshot does this by construction -- its Prepare only looks for a
// local file -- while every overlaybd create fetched the manifest first, so the same node
// started nothing. This is that regression, tested by taking the registry away.
func TestCreateSurvivesTheRegistryGoingDown(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("a1", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	p := obdLazyProvider(t, reg, newCountingBlobs())

	// First create with the registry up: this is what records the chain.
	if _, _, _, err := p.lowersFor(ctx, ref, false); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Registry gone. Pointing the client at a closed listener rather than stopping the
	// server, so the failure is a connection error like a real outage.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	p.Registry = NewRegistry(nil)
	p.Registry.Client = &http.Client{Transport: redirectTo{host: strings.TrimPrefix(deadURL, "http://")}}

	lowers, _, _, err := p.lowersFor(ctx, ref, false)
	if err != nil {
		t.Fatalf("create with the registry down: %v", err)
	}
	if len(lowers) != 1 {
		t.Fatalf("got %d lowers, want 1", len(lowers))
	}
	if lowers[0].Digest != fake.digest {
		t.Errorf("layer digest = %q, want the recorded %q", lowers[0].Digest, fake.digest)
	}
	// The config has to survive too, or the sandbox boots without ENV or ENTRYPOINT.
	// It lives in its own blob, so a manifest answered locally still leaves a registry
	// fetch on the path unless that is handled as well.
	cfg, err := p.Config(ref)
	if err != nil {
		t.Fatalf("config with the registry down: %v", err)
	}
	if cfg == nil {
		t.Error("no config available offline; the guest would start with no ENV or ENTRYPOINT")
	}
}

// A prewarm must not fall back. Its job is to bring the node up to date with the
// registry, so one that "succeeded" from a local record would report an image as freshly
// warmed while having asked nobody -- and would publish a chain it never verified.
func TestPrewarmFailsWhenTheRegistryIsDown(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("a2", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	p := obdLazyProvider(t, reg, newCountingBlobs())
	if err := p.Prewarm(ctx, ref); err != nil {
		t.Fatalf("first prewarm: %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	p.Registry = NewRegistry(nil)
	p.Registry.Client = &http.Client{Transport: redirectTo{host: strings.TrimPrefix(deadURL, "http://")}}

	if err := p.Prewarm(ctx, ref); err == nil {
		t.Error("prewarm succeeded with the registry down; it would report an image as " +
			"warmed without having checked anything")
	}
}

// A digest reference is resolvable with no registry at all: the digest is the answer a
// manifest fetch would give, and a manifest is immutable for a given digest.
//
// A tag deliberately does not get this. Asserted here so the distinction is not quietly
// lost later: pinning a tag to its last resolution is a create that succeeds while
// running the wrong image.
func TestDigestReferenceResolvesWithoutTheRegistryButATagDoesNot(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	manifestDigest := "sha256:" + strings.Repeat("b1", 32)
	layerDigest := "sha256:" + strings.Repeat("b2", 32)
	var manifestFetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/manifests/"):
			manifestFetches.Add(1)
			w.Header().Set("Content-Type", mediaTypeManifestV2)
			fmt.Fprintf(w, `{"schemaVersion":2,"mediaType":%q,`+
				`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg","size":2},`+
				`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
				mediaTypeManifestV2, layerDigest, len(layerGz))
		case strings.Contains(req.URL.Path, "/blobs/sha256:cfg"):
			w.Write([]byte(`{}`))
		case strings.Contains(req.URL.Path, "/blobs/"):
			w.Write(layerGz)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	reg := NewRegistry(nil)
	reg.Client = srv.Client()
	reg.Client.Transport = rewriteToHTTP{base: srv.Client().Transport}
	host := strings.TrimPrefix(srv.URL, "http://")
	p := obdLazyProvider(t, reg, newCountingBlobs())

	byDigest := host + "/test/img@" + manifestDigest
	if _, _, _, err := p.lowersFor(ctx, byDigest, false); err != nil {
		t.Fatalf("first create by digest: %v", err)
	}
	afterFirst := manifestFetches.Load()
	if _, _, _, err := p.lowersFor(ctx, byDigest, false); err != nil {
		t.Fatalf("second create by digest: %v", err)
	}
	if got := manifestFetches.Load(); got != afterFirst {
		t.Errorf("a digest reference fetched the manifest again (%d extra)", got-afterFirst)
	}

	byTag := host + "/test/img:latest"
	if _, _, _, err := p.lowersFor(ctx, byTag, false); err != nil {
		t.Fatalf("create by tag: %v", err)
	}
	beforeRepeat := manifestFetches.Load()
	if _, _, _, err := p.lowersFor(ctx, byTag, false); err != nil {
		t.Fatalf("repeat create by tag: %v", err)
	}
	if got := manifestFetches.Load(); got == beforeRepeat {
		t.Error("a tag was resolved from the local record; a moved tag would never be noticed")
	}
}

// redirectTo sends every request to one host, so a test can simulate the registry being
// unreachable without touching the reference the caller uses.
type redirectTo struct{ host string }

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = r.host
	return http.DefaultTransport.RoundTrip(req)
}

// The point of the index: a node that has never seen the image resolves it from the store
// with no registry at all. Publishing layer blobs alone does not achieve this -- a prefix
// of `blobs/sha256:...` says nothing about which of them form an image -- so before the
// index every such create went to the registry first.
//
// The second node's registry is pointed at a closed listener rather than omitted, so a
// resolution that reached for it fails loudly instead of being quietly skipped.
func TestASecondNodeResolvesFromTheStoreWithNoRegistry(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("c2", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	blobs := newCountingBlobs()
	index := newFakeIndex()

	// First node: converts, publishes the layers, and records what the image is.
	first := obdLazyProvider(t, reg, blobs)
	first.Index = index
	if err := first.Prewarm(ctx, ref); err != nil {
		t.Fatalf("prewarm: %v", err)
	}
	if index.manifests == 0 || index.tags == 0 {
		t.Fatalf("prewarm recorded %d manifests and %d tags; the store is still just a "+
			"layer cache", index.manifests, index.tags)
	}

	// Second node: empty layer dir, and a registry that refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	deadReg := NewRegistry(nil)
	deadReg.Client = &http.Client{Transport: redirectTo{host: strings.TrimPrefix(deadURL, "http://")}}

	second := obdLazyProvider(t, deadReg, blobs)
	second.Index = index
	lowers, _, _, err := second.lowersFor(ctx, ref, false)
	if err != nil {
		t.Fatalf("second node could not resolve from the store: %v", err)
	}
	if len(lowers) != 1 {
		t.Fatalf("got %d lowers, want 1", len(lowers))
	}
	if lowers[0].File != "" {
		t.Errorf("layer referenced as a local file %q; the second node holds nothing", lowers[0].File)
	}
	if lowers[0].RepoBlobURL != blobs.BlobURL() {
		t.Errorf("repoBlobUrl = %q, want the store's %q", lowers[0].RepoBlobURL, blobs.BlobURL())
	}
	// The size has to be the sealed blob's, since that is what a range read is measured
	// against.
	if want := blobs.blobs[fake.digest]; lowers[0].Size != want {
		t.Errorf("size = %d, want the sealed %d", lowers[0].Size, want)
	}
	// And the config, or the sandbox starts with no ENV or ENTRYPOINT. This is the part
	// that fails if the store records only the layer list.
	cfg, err := second.Config(ref)
	if err != nil {
		t.Fatalf("config on the second node: %v", err)
	}
	if cfg == nil {
		t.Error("no config on the second node; the guest would start with no ENV or ENTRYPOINT")
	}
}

// A prewarm must not answer from the store. Its job is to refresh the store against the
// registry, so reading its own previous answer would make it a no-op reporting success --
// and would let a moved tag never be picked up at all.
func TestPrewarmDoesNotResolveFromTheStore(t *testing.T) {
	requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTestTar(tarPath); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip layer: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("c3", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	index := newFakeIndex()

	first := obdLazyProvider(t, reg, newCountingBlobs())
	first.Index = index
	if err := first.Prewarm(ctx, ref); err != nil {
		t.Fatalf("prewarm: %v", err)
	}

	// A fresh node with the index populated but no registry: a create would succeed
	// here, and a prewarm must not.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	deadReg := NewRegistry(nil)
	deadReg.Client = &http.Client{Transport: redirectTo{host: strings.TrimPrefix(deadURL, "http://")}}

	second := obdLazyProvider(t, deadReg, newCountingBlobs())
	second.Index = index
	if err := second.Prewarm(ctx, ref); err == nil {
		t.Error("prewarm succeeded from the store; it would report an image as freshly " +
			"warmed without having asked the registry anything")
	}
}

// fakeIndex is an in-memory ImageIndex, counting writes so a test can tell "recorded the
// image" from "happened to work".
type fakeIndex struct {
	mu        sync.Mutex
	byDigest  map[string]*StoredManifest
	byTag     map[string]string
	manifests int
	tags      int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{byDigest: map[string]*StoredManifest{}, byTag: map[string]string{}}
}

func (f *fakeIndex) PutManifest(_ context.Context, digest string, m *StoredManifest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byDigest[digest] = m
	f.manifests++
	return nil
}

func (f *fakeIndex) GetManifest(_ context.Context, digest string) (*StoredManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byDigest[digest], nil
}

func (f *fakeIndex) PutTag(_ context.Context, ref Reference, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byTag[ref.Host+"/"+ref.Repository+":"+ref.Tag] = digest
	f.tags++
	return nil
}

func (f *fakeIndex) GetTag(_ context.Context, ref Reference) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byTag[ref.Host+"/"+ref.Repository+":"+ref.Tag], nil
}

// A configured disk size must be usable, not merely mountable.
//
// Two sizes are decided independently: the device is sized from the request, while the
// filesystem lives in the base layer and was sized by vsizeForImage when that layer was
// converted -- an estimate with a 2 GB floor. Nothing reconciled them, so a sandbox
// asking for less than the estimate got a device smaller than its own filesystem and
// the kernel refused to mount it: "bad geometry: block count 524288 exceeds size of
// device (262144 blocks)". Measured before the fix: 1024 MiB failed, 2048 and 4096
// worked -- and the default --default-disk-mib is 2048, exactly the floor, which is why
// every end-to-end run passed while any smaller disk was unusable.
//
// Capacity is asserted, not just the mount, because a filesystem can mount and still
// report the old size: that is the failure this would otherwise hide, where a caller
// configures 20 GB and gets 2.
func TestConfiguredSizeIsUsable(t *testing.T) {
	builder := requireOverlaybd(t)
	ctx := context.Background()
	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTar(tarPath, map[string]string{"f": "x\n"}); err != nil {
		t.Fatal(err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1024 is below vsizeForImage's floor (the case that used to fail), 4096 above it.
	for _, mib := range []int64{1024, 2048, 4096} {
		t.Run(fmt.Sprintf("%dMiB", mib), func(t *testing.T) {
			fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("ab", 32), layerGz: layerGz}
			ref, reg := fake.serve(t)
			p := NewOverlaybdProvider(t.TempDir(), builder.LayerDir, t.TempDir(), reg, builder, 512)
			r, err := p.Prepare(ctx, fmt.Sprintf("sbxsz%d", mib), ref, PrepareOptions{SizeMiB: mib})
			if err != nil {
				t.Fatalf("prepare %d MiB: %v", mib, err)
			}
			defer r.Release()

			real_, _ := os.Readlink(r.Device)
			at := t.TempDir()
			out, err := exec.Command("mount", "-t", "ext4", real_, at).CombinedOutput()
			if err != nil {
				t.Fatalf("%d MiB: cannot mount its own rootfs: %s",
					mib, strings.TrimSpace(string(out)))
			}
			defer func() { _ = exec.Command("umount", at).Run() }()

			var st syscall.Statfs_t
			if err := syscall.Statfs(at, &st); err != nil {
				t.Fatalf("statfs: %v", err)
			}
			totalMiB := int64(st.Blocks) * int64(st.Bsize) / (1 << 20)
			// ext4 metadata and the reserved block percentage take a cut, so the
			// filesystem is always somewhat smaller than the device. 85% catches "the
			// resize did not happen" (which would leave it at 2048 regardless of the
			// request) without asserting on ext4's exact overhead.
			if want := mib * 85 / 100; totalMiB < want {
				t.Errorf("%d MiB requested, filesystem reports %d MiB (want >= %d): "+
					"the resize did not take", mib, totalMiB, want)
			}
			t.Logf("%d MiB requested -> filesystem %d MiB", mib, totalMiB)

			// Written, not merely reported: a size that statfs claims but the device
			// cannot hold would fail here instead.
			target := mib * 60 / 100
			big := filepath.Join(at, "fill")
			if out, err := exec.Command("dd", "if=/dev/zero", "of="+big,
				"bs=1M", fmt.Sprintf("count=%d", target)).CombinedOutput(); err != nil {
				t.Errorf("%d MiB: could not write %d MiB: %v: %s",
					mib, target, err, strings.TrimSpace(string(out)))
				return
			}
			if err := exec.Command("sync").Run(); err != nil {
				t.Fatalf("sync: %v", err)
			}
			info, err := os.Stat(big)
			if err != nil {
				t.Fatalf("stat written file: %v", err)
			}
			if got := info.Size() / (1 << 20); got != target {
				t.Errorf("wrote %d MiB, file is %d MiB", target, got)
			}
			t.Logf("%d MiB requested -> wrote %d MiB successfully", mib, target)
		})
	}
}

// Two sandboxes from one image must not see each other's writes. This is the
// question a container tier turns on: runc needs a mounted directory tree rather
// than a block device, and if the isolation came from the VM boundary rather than
// from the layer arrangement, mounting the same device twice would share writes.
//
// Checked by mounting both devices on the host and writing through one, which is
// exactly what a container tier would do -- so a pass here is evidence about that
// path and not only about the microVM one. Writes are synced before the comparison,
// or a pass would only show that A's dirty pages had not reached B's cache yet.
//
// Two layers of protection turned up while confirming this test can fail. Pointing
// both mounts at one device produces exactly the leak the assertions name, and
// giving two sandboxes the same writable directory does not silently share it --
// overlaybd-create refuses with "File exists" before a device is ever assembled.
func TestWritableLayersAreIsolatedAcrossSandboxes(t *testing.T) {
	builder := requireOverlaybd(t)
	ctx := context.Background()

	tarPath := filepath.Join(t.TempDir(), "layer.tar")
	if err := writeTar(tarPath, map[string]string{"shared": "from the image\n"}); err != nil {
		t.Fatalf("build tar: %v", err)
	}
	layerGz, err := gzipFile(tarPath)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}

	fake := &obdTestRegistry{digest: "sha256:" + strings.Repeat("f5", 32), layerGz: layerGz}
	ref, reg := fake.serve(t)
	p := NewOverlaybdProvider(t.TempDir(), builder.LayerDir, t.TempDir(), reg, builder, 512)

	// Two sandboxes, same image.
	first, err := p.Prepare(ctx, "sbxaaa1", ref, PrepareOptions{SizeMiB: 2048})
	if err != nil {
		t.Fatalf("prepare first: %v", err)
	}
	defer first.Release()
	second, err := p.Prepare(ctx, "sbxbbb2", ref, PrepareOptions{SizeMiB: 2048})
	if err != nil {
		t.Fatalf("prepare second: %v", err)
	}
	defer second.Release()

	if first.Device == second.Device {
		t.Fatalf("both sandboxes got the same device %q; writes cannot be isolated", first.Device)
	}
	t.Logf("devices: %s and %s", first.Device, second.Device)
	if first.Writable == second.Writable {
		t.Errorf("both sandboxes share writable layer %q", first.Writable)
	}

	mountA := t.TempDir()
	mountB := t.TempDir()
	for _, m := range []struct {
		dev, at string
	}{{first.Device, mountA}, {second.Device, mountB}} {
		real_, _ := os.Readlink(m.dev)
		out, err := exec.Command("mount", "-t", "ext4", real_, m.at).CombinedOutput()
		if err != nil {
			t.Fatalf("mount %s (-> %s): %v: %s", m.dev, real_, err, out)
		}
		at := m.at
		defer func() { _ = exec.Command("umount", at).Run() }()
	}

	// Both must start from the image.
	for _, at := range []string{mountA, mountB} {
		b, err := os.ReadFile(filepath.Join(at, "shared"))
		if err != nil {
			t.Fatalf("read image file at %s: %v", at, err)
		}
		if string(b) != "from the image\n" {
			t.Errorf("image content at %s = %q", at, b)
		}
	}

	// Write through A only.
	if err := os.WriteFile(filepath.Join(mountA, "only-in-a"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write in A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountA, "shared"), []byte("overwritten by a\n"), 0o644); err != nil {
		t.Fatalf("overwrite in A: %v", err)
	}
	// Flushed to the device, not merely to the page cache: a pass that only reflects
	// dirty pages in A's own cache would say nothing about what B's device holds.
	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}

	// B must see neither.
	if _, err := os.Stat(filepath.Join(mountB, "only-in-a")); err == nil {
		t.Error("B sees a file created in A; the writable layers are shared")
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking B: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(mountB, "shared"))
	if err != nil {
		t.Fatalf("read shared in B: %v", err)
	}
	if string(b) != "from the image\n" {
		t.Errorf("B sees %q; A's overwrite leaked", b)
	}

	// And A must still see its own writes -- otherwise the isolation is really just
	// both devices ignoring writes.
	if b, err := os.ReadFile(filepath.Join(mountA, "shared")); err != nil {
		t.Fatalf("re-read in A: %v", err)
	} else if string(b) != "overwritten by a\n" {
		t.Errorf("A does not see its own write: %q", b)
	}
}
