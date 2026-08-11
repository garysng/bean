package image

import (
	"context"
	"testing"
)

// The index is what turns the store from a layer cache into something an image can be
// resolved from. So what matters is the round trip a resolving node makes -- tag to
// digest to layers -- and the failure modes that would otherwise surface as a create
// failing for reasons naming zfile structure.

func indexFor(t *testing.T) (*fakePutter, ImageIndex) {
	t.Helper()
	f := newFakePutter()
	idx, err := NewS3ImageIndex(f)
	if err != nil {
		t.Fatal(err)
	}
	return f, idx
}

func TestIndexResolvesATagThroughToLayers(t *testing.T) {
	_, idx := indexFor(t)
	ctx := context.Background()
	ref := Reference{Host: "registry-1.docker.io", Repository: "library/python", Tag: "3.11-slim"}
	digest := "sha256:78b39ef14d8e"

	stored := &StoredManifest{
		Digest: digest,
		Layers: []StoredLayer{
			{Digest: "sha256:aaa", Size: 48859648, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"},
			{Digest: "sha256:bbb", Size: 3060736},
		},
		Config: &Config{Env: []string{"PATH=/usr/bin"}, Entrypoint: []string{"python3"}},
	}
	if err := idx.PutManifest(ctx, digest, stored); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if err := idx.PutTag(ctx, ref, digest); err != nil {
		t.Fatalf("PutTag: %v", err)
	}

	// The walk a node with no local copy makes.
	got, err := idx.GetTag(ctx, ref)
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if got != digest {
		t.Fatalf("GetTag = %q, want %q", got, digest)
	}
	m, err := idx.GetManifest(ctx, got)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if m == nil {
		t.Fatal("GetManifest returned nothing for a manifest just written")
	}
	if len(m.Layers) != 2 {
		t.Fatalf("got %d layers, want 2", len(m.Layers))
	}
	// The sealed size, not the manifest's figure for the original OCI blob: a remote
	// layer is range-read against this, and a wrong value reads past the end.
	if m.Layers[0].Size != 48859648 {
		t.Errorf("layer size = %d, want the sealed 48859648", m.Layers[0].Size)
	}
	// The config has to come back too, or resolving offline gets as far as the layers
	// and then goes to the registry for the config anyway.
	if m.Config == nil || len(m.Config.Entrypoint) != 1 {
		t.Errorf("config did not survive the round trip: %+v", m.Config)
	}
}

// A digest reference skips the tag entirely, which is the case that needs no mutable
// pointer at all.
func TestIndexResolvesADigestWithoutATag(t *testing.T) {
	_, idx := indexFor(t)
	ctx := context.Background()
	digest := "sha256:abc123"

	if err := idx.PutManifest(ctx, digest, &StoredManifest{
		Digest: digest,
		Layers: []StoredLayer{{Digest: "sha256:aaa", Size: 10}},
	}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	m, err := idx.GetManifest(ctx, digest)
	if err != nil || m == nil {
		t.Fatalf("GetManifest = %v, %v", m, err)
	}
}

// An empty store is the normal state before the first prewarm, and a miss must read as
// "ask the registry" rather than as an error -- otherwise adding an index would make
// un-prewarmed images fail instead of falling back.
func TestIndexReportsMissesAsAbsentRatherThanFailing(t *testing.T) {
	_, idx := indexFor(t)
	ctx := context.Background()

	m, err := idx.GetManifest(ctx, "sha256:never-written")
	if err != nil {
		t.Errorf("GetManifest on a miss returned an error: %v", err)
	}
	if m != nil {
		t.Error("GetManifest invented a manifest")
	}

	digest, err := idx.GetTag(ctx, Reference{Host: "h", Repository: "r", Tag: "nope"})
	if err != nil {
		t.Errorf("GetTag on a miss returned an error: %v", err)
	}
	if digest != "" {
		t.Errorf("GetTag = %q on a miss", digest)
	}
}

// A corrupt answer is not a miss. Using it would build a device whose reads fail with an
// error naming zfile structure, saying nothing about the damaged object behind it, so it
// propagates instead.
func TestIndexRejectsDamagedObjects(t *testing.T) {
	f, idx := indexFor(t)
	ctx := context.Background()

	f.objects["manifests/sha256:bad"] = []byte("{not json")
	if _, err := idx.GetManifest(ctx, "sha256:bad"); err == nil {
		t.Error("GetManifest accepted unparseable JSON")
	}

	// A manifest listing no layers assembles nothing, and would otherwise be handed
	// back as a usable answer.
	f.objects["manifests/sha256:empty"] = []byte(`{"digest":"sha256:empty","layers":[]}`)
	if _, err := idx.GetManifest(ctx, "sha256:empty"); err == nil {
		t.Error("GetManifest accepted a manifest with no layers")
	}

	// A tag pointing at something that is not a digest would be used as a manifest key,
	// producing a miss that looks like "image not published".
	f.objects["tags/h/r/latest"] = []byte("not-a-digest")
	if _, err := idx.GetTag(ctx, Reference{Host: "h", Repository: "r", Tag: "latest"}); err == nil {
		t.Error("GetTag accepted a value that is not a digest")
	}
}

// A tag is only meaningful with its host and repository: python:3.12 from Docker Hub and
// from a mirror are different images sharing a name, and one key for both would have one
// serve the other.
func TestIndexKeysTagsByHostAndRepository(t *testing.T) {
	f, idx := indexFor(t)
	ctx := context.Background()

	hub := Reference{Host: "registry-1.docker.io", Repository: "library/python", Tag: "3.12"}
	mirror := Reference{Host: "mirror.example", Repository: "library/python", Tag: "3.12"}
	if err := idx.PutTag(ctx, hub, "sha256:fromhub"); err != nil {
		t.Fatal(err)
	}
	if err := idx.PutTag(ctx, mirror, "sha256:frommirror"); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetTag(ctx, hub)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:fromhub" {
		t.Errorf("hub tag resolved to %q; the two registries share a key", got)
	}
	if len(f.objects) != 2 {
		t.Errorf("wrote %d objects, want 2 distinct keys", len(f.objects))
	}
}

// A tag is a mutable pointer, so an update has to overwrite. Refusing or duplicating
// would leave the store serving the old digest after a prewarm that saw a new one.
func TestIndexTagUpdateOverwrites(t *testing.T) {
	_, idx := indexFor(t)
	ctx := context.Background()
	ref := Reference{Host: "h", Repository: "r", Tag: "latest"}

	if err := idx.PutTag(ctx, ref, "sha256:old"); err != nil {
		t.Fatal(err)
	}
	if err := idx.PutTag(ctx, ref, "sha256:new"); err != nil {
		t.Fatal(err)
	}
	got, err := idx.GetTag(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:new" {
		t.Errorf("tag = %q after an update, want sha256:new", got)
	}
}
