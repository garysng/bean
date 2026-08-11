package image

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/garysng/bean/internal/control/s3"
)

// Publishing layer blobs alone does not make the store a source an image can be resolved
// from. A prefix full of `blobs/sha256:...` is a flat set of layers with nothing saying
// which of them make up an image, so a node still has to ask the registry -- and then the
// store is a cache, not a source.
//
// This file adds the two things that were missing:
//
//	manifests/<manifest-digest>   which layers, and the image's config
//	tags/<host>/<repo>/<tag>      which manifest digest a tag points at
//
// With both, a node that has never seen the image resolves it from the store: read the
// tag, read the manifest, reference the layers. No registry.
//
// The consequence is that bean's store becomes the authority for what a tag means, and
// prewarm is what updates it. An upstream tag that moves is not noticed until the next
// prewarm. That is deliberate for a sandbox platform -- a batch of evals half-way through
// silently picking up new image contents is worse than running a slightly old image -- but
// it is a real semantic, not a caching artefact, which is why it is written down here and
// in docs/image-pipeline.md rather than left to be discovered.

// ImageIndex resolves and records what an image is made of, in the object store.
//
// Separate from BlobStore because the readers differ. Layer blobs are read by the
// overlaybd daemon, anonymously over HTTP, which is what forces the public-read policy on
// that prefix. These objects are read by bean itself with credentials, so they carry no
// such requirement -- and a deployment may reasonably want them private.
type ImageIndex interface {
	// PutManifest records an image's layer list and config under its manifest digest.
	PutManifest(ctx context.Context, digest string, m *StoredManifest) error
	// GetManifest reads back what PutManifest wrote, or nil if it is not there.
	GetManifest(ctx context.Context, digest string) (*StoredManifest, error)
	// PutTag points a reference at a manifest digest.
	PutTag(ctx context.Context, ref Reference, digest string) error
	// GetTag reports which manifest digest a reference points at, or "" if unknown.
	GetTag(ctx context.Context, ref Reference) (string, error)
}

// StoredManifest is an image reduced to what resolving it needs.
//
// Not the registry's manifest bytes, deliberately. Those would have to be re-parsed, and
// their layer sizes describe the *original* OCI blobs while what the store holds is the
// sealed overlaybd form -- a difference that matters because a remote layer is range-read
// against its declared length. So this records the sealed sizes, which is what a reader
// here actually needs.
type StoredManifest struct {
	// Digest is the manifest digest, repeated inside the object so a reader that got
	// here via a tag can check it landed where it meant to.
	Digest string `json:"digest"`
	// Layers is the chain, base first.
	Layers []StoredLayer `json:"layers"`
	// Config is the image's OCI configuration, so a resolving node does not need the
	// registry for it either. Without this the manifest would resolve offline and the
	// config fetch would still go out, which is no better.
	Config *Config `json:"config,omitempty"`
}

// StoredLayer is one layer as the store holds it.
type StoredLayer struct {
	// Digest is the OCI layer digest, which is also the blob key: the sealed form is
	// published under the digest of the layer it came from, so one digest identifies
	// both.
	Digest string `json:"digest"`
	// Size is the *sealed* layer's length, not the original OCI blob's. Sealing
	// recompresses, so the two differ -- measured 48859648 against a declared 29780765 --
	// and a remote layer read against the wrong length stops short or runs past the end.
	Size int64 `json:"size"`
	// MediaType is the original layer's, kept because it decides whether a blob needs
	// converting at all.
	MediaType string `json:"mediaType,omitempty"`
}

// s3ImageIndex stores manifests and tags in the same bucket as the layer blobs.
//
// One bucket rather than two because they share a lifecycle: a layer without its manifest
// is unreferenced, and a manifest whose layers were reclaimed is a promise the store
// cannot keep. Keeping them together means one thing to configure and one thing to
// garbage-collect.
type s3ImageIndex struct {
	store s3.ObjectStore
}

func NewS3ImageIndex(store s3.ObjectStore) (ImageIndex, error) {
	if store == nil {
		return nil, fmt.Errorf("image: image index needs an object store")
	}
	return &s3ImageIndex{store: store}, nil
}

func (s *s3ImageIndex) manifestKey(digest string) string { return "manifests/" + digest }

// tagKey names a tag object.
//
// Host and repository are part of the key because a tag is only meaningful with them:
// `python:3.12` from Docker Hub and from a mirror are different images that happen to
// share a name, and collapsing them would have one serve the other.
func (s *s3ImageIndex) tagKey(ref Reference) string {
	return "tags/" + ref.Host + "/" + ref.Repository + "/" + ref.Tag
}

func (s *s3ImageIndex) PutManifest(ctx context.Context, digest string, m *StoredManifest) error {
	if digest == "" || m == nil {
		return fmt.Errorf("image: manifest needs a digest")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := s3.Put(ctx, s.store, s.manifestKey(digest), bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("image: record manifest %s: %w", digest, err)
	}
	return nil
}

func (s *s3ImageIndex) GetManifest(ctx context.Context, digest string) (*StoredManifest, error) {
	r, err := s.store.Get(ctx, s.manifestKey(digest))
	if err != nil {
		// Absent and unreachable are both reported as "not there". The caller's next
		// move is the registry either way, and distinguishing them would only offer a
		// choice it does not have.
		return nil, nil
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, nil
	}
	var m StoredManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("image: stored manifest %s is unreadable: %w", digest, err)
	}
	if len(m.Layers) == 0 {
		// A manifest with no layers cannot assemble anything, and would otherwise be
		// returned as a usable answer that fails later with a less clear error.
		return nil, fmt.Errorf("image: stored manifest %s lists no layers", digest)
	}
	return &m, nil
}

func (s *s3ImageIndex) PutTag(ctx context.Context, ref Reference, digest string) error {
	if ref.Tag == "" || digest == "" {
		return fmt.Errorf("image: tag pointer needs a tag and a digest")
	}
	if err := s3.Put(ctx, s.store, s.tagKey(ref), strings.NewReader(digest), int64(len(digest))); err != nil {
		return fmt.Errorf("image: record tag %s: %w", ref.Tag, err)
	}
	return nil
}

func (s *s3ImageIndex) GetTag(ctx context.Context, ref Reference) (string, error) {
	if ref.Tag == "" {
		return "", nil
	}
	r, err := s.store.Get(ctx, s.tagKey(ref))
	if err != nil {
		return "", nil
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return "", nil
	}
	digest := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(digest, "sha256:") {
		// Checked because a truncated or wrong object would otherwise be used as a
		// manifest key, producing a miss that looks like "image not published".
		return "", fmt.Errorf("image: tag %s points at %q, which is not a digest", ref.Tag, digest)
	}
	return digest, nil
}
