package image

import (
	"os"
	"path/filepath"
	"testing"
)

// The sidecar mechanism is what makes a node's image cache visible to the
// scheduler, so its failure modes matter: reporting an image that is not usable
// would send placement to a node that then has to pull anyway, and reporting
// nothing makes image affinity silently useless.

// seedCachedImage writes an image file and its sidecar, as a conversion does.
func seedCachedImage(t *testing.T, dir, ref string, size int64) {
	t.Helper()
	name, err := refToFilename(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := createSparse(filepath.Join(dir, name+".ext4"), size); err != nil {
		t.Fatal(err)
	}
	if err := recordRef(dir, ref); err != nil {
		t.Fatal(err)
	}
}

func TestCachedImagesReportsRefsAndSizes(t *testing.T) {
	dir := t.TempDir()
	seedCachedImage(t, dir, "alpine:3.20", 64)
	seedCachedImage(t, dir, "ghcr.io/owner/img:v1", 128)

	got, err := cachedImages(dir)
	if err != nil {
		t.Fatalf("cachedImages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("reported %d images, want 2: %v", len(got), got)
	}
	// The reference has to survive the round trip: the filename encoding is not
	// reversible, which is why the sidecar exists at all.
	if got["alpine:3.20"] != 64<<20 {
		t.Errorf("alpine size = %d, want %d", got["alpine:3.20"], 64<<20)
	}
	if got["ghcr.io/owner/img:v1"] != 128<<20 {
		t.Errorf("ghcr size = %d", got["ghcr.io/owner/img:v1"])
	}
}

func TestCachedImagesOnMissingDirectory(t *testing.T) {
	got, err := cachedImages(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a node with no images yet is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reported %d images from a missing directory", len(got))
	}
}

// TestCachedImagesSkipsSidecarWithoutImage covers a deleted image whose sidecar
// remains. Reporting it would advertise an image the node cannot actually use.
func TestCachedImagesSkipsSidecarWithoutImage(t *testing.T) {
	dir := t.TempDir()
	seedCachedImage(t, dir, "gone:1", 64)

	name, err := refToFilename("gone:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, name+".ext4")); err != nil {
		t.Fatal(err)
	}

	got, err := cachedImages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, reported := got["gone:1"]; reported {
		t.Error("reported an image whose file is gone")
	}
}

// TestCachedImagesSkipsImageWithoutSidecar covers the reverse: an image file
// with no record of its reference. Guessing the reference from the filename is
// impossible, so it is skipped rather than reported wrongly.
func TestCachedImagesSkipsImageWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := createSparse(filepath.Join(dir, "mystery.ext4"), 64); err != nil {
		t.Fatal(err)
	}

	got, err := cachedImages(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("reported %v for an image with no sidecar", got)
	}
}

func TestCachedImagesIgnoresMalformedSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := createSparse(filepath.Join(dir, "broken.ext4"), 64); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"not json", "{}", `{"ref":""}`} {
		if err := os.WriteFile(filepath.Join(dir, "broken"+refSuffix),
			[]byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := cachedImages(dir)
		if err != nil {
			t.Fatalf("content %q: %v", content, err)
		}
		if len(got) != 0 {
			t.Errorf("content %q reported %v", content, got)
		}
	}
}

// TestCachedRefsCachesUntilInvalidated matters because a heartbeat fires every
// few seconds: scanning the image directory each time would be wasteful, but a
// stale list means the scheduler never learns about a new image.
func TestCachedRefsCachesUntilInvalidated(t *testing.T) {
	dir := t.TempDir()
	seedCachedImage(t, dir, "first:1", 64)

	var c cachedRefs
	got, err := c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("initial read = %v", got)
	}

	// A new image is not seen until the cache is told about it.
	seedCachedImage(t, dir, "second:1", 64)
	got, err = c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("cache re-scanned the directory; got %v", got)
	}

	c.invalidate()
	got, err = c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("after invalidation = %v, want both images", got)
	}
}

// TestCachedRefsReturnsACopy guards against a caller mutating the cache, which
// would corrupt what every later heartbeat reports.
func TestCachedRefsReturnsACopy(t *testing.T) {
	dir := t.TempDir()
	seedCachedImage(t, dir, "img:1", 64)

	var c cachedRefs
	first, err := c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	first["injected:1"] = 999
	delete(first, "img:1")

	second, err := c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, injected := second["injected:1"]; injected {
		t.Error("mutating the returned map changed the cache")
	}
	if _, ok := second["img:1"]; !ok {
		t.Error("deleting from the returned map changed the cache")
	}
}

// TestRecordRefIsAtomic checks a sidecar is never left half-written: a truncated
// one would make a perfectly usable image invisible.
func TestRecordRefIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := recordRef(dir, "atomic:1"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}

	// Recording again must overwrite cleanly rather than fail.
	if err := recordRef(dir, "atomic:1"); err != nil {
		t.Errorf("second recordRef: %v", err)
	}
}

func TestRecordRefRejectsEmptyRef(t *testing.T) {
	if err := recordRef(t.TempDir(), ""); err == nil {
		t.Error("recordRef accepted an empty reference")
	}
}
