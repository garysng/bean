package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := recordRef(dir, ref, "", nil); err != nil {
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
	if got["alpine:3.20"].SizeBytes != 64<<20 {
		t.Errorf("alpine size = %d, want %d", got["alpine:3.20"].SizeBytes, 64<<20)
	}
	if got["ghcr.io/owner/img:v1"].SizeBytes != 128<<20 {
		t.Errorf("ghcr size = %d", got["ghcr.io/owner/img:v1"].SizeBytes)
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

// TestCachedRefsSeesNewImagesWithoutBeingTold is the property that replaced
// explicit invalidation. Every writer publishes into this directory, so
// detecting the change here means a new image cannot be invisible because a
// caller forgot to invalidate — which is exactly how a built image stayed hidden
// from the scheduler until its node restarted.
func TestCachedRefsSeesNewImagesWithoutBeingTold(t *testing.T) {
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

	// The directory's mtime has one-second granularity on some filesystems, so
	// the write has to be distinguishable from the read that preceded it.
	time.Sleep(1100 * time.Millisecond)
	seedCachedImage(t, dir, "second:1", 64)

	got, err = c.get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("after a new image = %v, want both", got)
	}
}

// TestCachedRefsAvoidsRescanningUnchangedDirectory checks the caching actually
// happens: a heartbeat every few seconds should not re-read the directory.
func TestCachedRefsAvoidsRescanningUnchangedDirectory(t *testing.T) {
	dir := t.TempDir()
	seedCachedImage(t, dir, "stable:1", 64)

	var c cachedRefs
	if _, err := c.get(dir); err != nil {
		t.Fatal(err)
	}
	before := c.stamp

	if _, err := c.get(dir); err != nil {
		t.Fatal(err)
	}
	if !c.stamp.Equal(before) {
		t.Error("re-scanned an unchanged directory")
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
	first["injected:1"] = CachedImage{SizeBytes: 999}
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
	if err := recordRef(dir, "atomic:1", "", nil); err != nil {
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
	if err := recordRef(dir, "atomic:1", "", nil); err != nil {
		t.Errorf("second recordRef: %v", err)
	}
}

func TestRecordRefRejectsEmptyRef(t *testing.T) {
	if err := recordRef(t.TempDir(), "", "", nil); err == nil {
		t.Error("recordRef accepted an empty reference")
	}
}

// TestCachedDigestRoundTrips covers the field warm snapshots key on.
func TestCachedDigestRoundTrips(t *testing.T) {
	dir := t.TempDir()
	const ref = "python:3.12"
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if err := recordRef(dir, ref, digest, nil); err != nil {
		t.Fatal(err)
	}
	got, err := cachedDigest(dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != digest {
		t.Errorf("cachedDigest = %q, want %q", got, digest)
	}
}

// TestCachedDigestOfAnImageWithoutOne is the compatibility path: an image
// converted before the digest was recorded, and a build or commit whose output
// never had a manifest.
//
// The empty return must not be an error. A caller looking up a warm snapshot
// treats it as a miss and boots, which is the same thing it does on a node whose
// CPU has no warm snapshot -- and if this errored instead, adding the field would
// have broken every image already on a node.
func TestCachedDigestOfAnImageWithoutOne(t *testing.T) {
	dir := t.TempDir()
	if err := recordRef(dir, "built:1", "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := cachedDigest(dir, "built:1")
	if err != nil {
		t.Fatalf("an image with no digest must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("cachedDigest = %q, want empty", got)
	}
}

// TestCachedDigestOfAnAbsentImage distinguishes "not here" from "here without a
// digest". Both return empty, and neither is an error, because the caller's
// response to both is the same: boot.
func TestCachedDigestOfAnAbsentImage(t *testing.T) {
	got, err := cachedDigest(t.TempDir(), "never:pulled")
	if err != nil {
		t.Fatalf("absent image must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("cachedDigest = %q, want empty", got)
	}
}

// TestMovedTagGetsADistinctDigest is the reason the field exists.
//
// Keying a warm snapshot on the reference would serve the environment captured
// from whatever the tag used to name, and it would do so silently: the wrong
// snapshot restores successfully. This asserts the recorded digest follows the
// image rather than the name, which is what makes the key safe.
func TestMovedTagGetsADistinctDigest(t *testing.T) {
	dir := t.TempDir()
	const tag = "app:latest"
	const before = "sha256:aaaa111111111111111111111111111111111111111111111111111111111111"
	const after = "sha256:bbbb222222222222222222222222222222222222222222222222222222222222"

	if err := recordRef(dir, tag, before, nil); err != nil {
		t.Fatal(err)
	}
	// The tag moves and the image is converted again under the same name.
	if err := recordRef(dir, tag, after, nil); err != nil {
		t.Fatal(err)
	}
	got, err := cachedDigest(dir, tag)
	if err != nil {
		t.Fatal(err)
	}
	if got == before {
		t.Errorf("cachedDigest still reports the pre-move digest %q; a warm "+
			"snapshot keyed on it would restore an environment captured from an "+
			"image this tag no longer names, and the restore would succeed", before)
	}
	if got != after {
		t.Errorf("cachedDigest = %q, want %q", got, after)
	}
}

// TestCachedDigestReportsACorruptSidecar keeps a damaged cache from hiding behind
// the slow path. An unparseable sidecar means the image is present but its
// identity is unknown, which is worth surfacing rather than treating as "no
// digest" and quietly booting forever.
func TestCachedDigestReportsACorruptSidecar(t *testing.T) {
	dir := t.TempDir()
	name, err := refToFilename("broken:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+refSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedDigest(dir, "broken:1"); err == nil {
		t.Error("a corrupt sidecar was reported as an image without a digest")
	}
}
