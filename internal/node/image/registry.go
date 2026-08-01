// Package image also fetches images from OCI registries.
//
// The node pulls images itself rather than shelling out to a container runtime.
// A sandbox's rootfs is a filesystem image, not a container snapshot, so the
// useful part of a runtime — its snapshotter, its own image store, its daemon —
// is not reusable here; what is needed is the manifest, the layer blobs, and
// somewhere to unpack them. Doing that directly also means a node has no
// dependency on docker or containerd being installed and healthy.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Registry fetches manifests and layer blobs over the OCI distribution API.
type Registry struct {
	// Client carries the timeouts and connection pooling a pull needs; layer
	// blobs can be hundreds of megabytes.
	Client *http.Client
	// Auth supplies credentials per registry host. Nil means anonymous only.
	Auth CredentialSource

	tokens tokenCache
}

// CredentialSource resolves a registry host to a basic-auth pair. The node gets
// these from the control plane rather than holding long-lived secrets on disk.
type CredentialSource interface {
	Credential(host string) (username, password string, ok bool)
}

func NewRegistry(auth CredentialSource) *Registry {
	return &Registry{
		Client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		Auth: auth,
	}
}

// Reference is a parsed image reference.
type Reference struct {
	// Host is the registry, defaulting to Docker Hub when the reference has
	// no registry component.
	Host string
	// Repository includes any namespace, e.g. "library/alpine".
	Repository string
	// Tag or Digest identifies the image; exactly one is set.
	Tag    string
	Digest string
}

// ParseReference reads the usual forms: "alpine", "alpine:3.20",
// "ghcr.io/owner/image:tag", "repo@sha256:...".
//
// The rules are inherited from Docker rather than chosen: a leading component is
// a registry host only if it contains a dot or colon, or is exactly "localhost",
// which is why "alpine/foo" is a Hub repository but "a.io/foo" is not.
func ParseReference(ref string) (Reference, error) {
	if ref == "" {
		return Reference{}, errors.New("image: reference required")
	}

	var out Reference
	rest := ref

	// A digest binds the reference to exact content, so it is taken first.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		out.Digest = rest[at+1:]
		rest = rest[:at]
		if !strings.HasPrefix(out.Digest, "sha256:") || len(out.Digest) != 71 {
			return Reference{}, fmt.Errorf("image: malformed digest in %q", ref)
		}
	}

	// The first path component is a registry host only if it looks like one.
	if slash := strings.Index(rest, "/"); slash >= 0 {
		candidate := rest[:slash]
		if strings.ContainsAny(candidate, ".:") || candidate == "localhost" {
			out.Host = candidate
			rest = rest[slash+1:]
		}
	}

	// A colon after the last slash is a tag; before it, it was a host port.
	if out.Digest == "" {
		if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon:], "/") {
			out.Tag = rest[colon+1:]
			rest = rest[:colon]
			if out.Tag == "" {
				return Reference{}, fmt.Errorf("image: empty tag in %q", ref)
			}
		} else {
			out.Tag = "latest"
		}
	}

	if rest == "" {
		return Reference{}, fmt.Errorf("image: no repository in %q", ref)
	}
	out.Repository = rest

	if out.Host == "" {
		out.Host = "registry-1.docker.io"
		// Hub puts unqualified names under "library".
		if !strings.Contains(out.Repository, "/") {
			out.Repository = "library/" + out.Repository
		}
	}
	return out, nil
}

// Manifest describes an image's layers and configuration.
type Manifest struct {
	Config Descriptor   `json:"config"`
	Layers []Descriptor `json:"layers"`
	// Digest is the manifest's own digest, which is the image's identity.
	Digest string `json:"-"`
}

// Descriptor points at one blob.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Config is the part of an image's configuration a sandbox needs. The rest of
// the OCI config describes container semantics the microVM tier does not use.
type Config struct {
	Env        []string `json:"Env"`
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
}

const (
	mediaTypeManifestV2   = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex     = "application/vnd.oci.image.index.v1+json"
)

// FetchManifest resolves a reference to a single-platform manifest, following a
// multi-platform index to the linux/amd64 entry.
func (r *Registry) FetchManifest(ctx context.Context, ref Reference) (*Manifest, error) {
	id := ref.Digest
	if id == "" {
		id = ref.Tag
	}

	body, contentType, digest, err := r.getManifest(ctx, ref, id)
	if err != nil {
		return nil, err
	}

	// A multi-platform index has to be resolved before the layers make sense.
	if contentType == mediaTypeManifestList || contentType == mediaTypeOCIIndex {
		target, err := pickPlatform(body)
		if err != nil {
			return nil, err
		}
		body, _, digest, err = r.getManifest(ctx, ref, target)
		if err != nil {
			return nil, fmt.Errorf("image: fetch platform manifest: %w", err)
		}
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("image: parse manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return nil, errors.New("image: manifest has no layers")
	}
	m.Digest = digest
	return &m, nil
}

// pickPlatform selects linux/amd64 from an index. The microVM tier boots an
// x86-64 kernel, so another architecture's layers would produce a rootfs whose
// binaries cannot run — better to fail here with a clear message.
func pickPlatform(index []byte) (string, error) {
	var idx struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return "", fmt.Errorf("image: parse manifest index: %w", err)
	}
	for _, m := range idx.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m.Digest, nil
		}
	}
	return "", errors.New("image: no linux/amd64 manifest in index")
}

func (r *Registry) getManifest(ctx context.Context, ref Reference, id string) (body []byte, contentType, digest string, err error) {
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s",
		ref.Host, ref.Repository, url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", "", err
	}
	// Every manifest type the platform can handle is offered, so the registry
	// does not fall back to the deprecated v1 schema.
	req.Header.Set("Accept", strings.Join([]string{
		mediaTypeManifestV2, mediaTypeManifestList,
		mediaTypeOCIManifest, mediaTypeOCIIndex,
	}, ", "))

	resp, err := r.do(ctx, req, ref)
	if err != nil {
		return nil, "", "", err
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", "", fmt.Errorf("image: %s/%s:%s not found",
			ref.Host, ref.Repository, id)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", statusError(resp, "fetch manifest")
	}

	body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", "", err
	}
	digest = resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		// Not all registries send the header, and the digest is the image's
		// identity, so it is computed rather than left empty.
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return body, resp.Header.Get("Content-Type"), digest, nil
}

// FetchBlob opens a blob for reading. The caller closes it.
//
// Layer blobs are large and usually served by a CDN redirect, where a reset
// mid-transfer is common enough that a single attempt makes conversion
// unreliable — observed as "connection reset by peer" partway through a layer.
// The attempt is therefore retried, and because a reset can also happen after
// the body has started streaming, the retry is at the reader rather than only
// around the request.
func (r *Registry) FetchBlob(ctx context.Context, ref Reference, digest string) (io.ReadCloser, error) {
	body, err := r.openBlob(ctx, ref, digest, 0)
	if err != nil {
		return nil, err
	}
	return &resumingBlob{
		reg: r, ctx: ctx, ref: ref, digest: digest, body: body,
	}, nil
}

// openBlob requests a blob, optionally from a byte offset so a broken transfer
// can resume rather than start over.
func (r *Registry) openBlob(ctx context.Context, ref Reference, digest string, offset int64) (io.ReadCloser, error) {
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Host, ref.Repository, digest)

	var lastErr error
	for attempt := 0; attempt < blobAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}

		resp, err := r.do(ctx, req, ref)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			// A registry that ignores the Range header restarts the blob, which
			// would corrupt a resumed layer. Detecting it is better than
			// silently producing a broken image.
			if offset > 0 && resp.StatusCode == http.StatusOK {
				closeBody(resp)
				return nil, fmt.Errorf("image: %s does not support resuming blob %s", ref.Host, digest)
			}
			return resp.Body, nil
		}
		// 4xx other than 429 will not improve on retry.
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			defer closeBody(resp)
			return nil, statusError(resp, "fetch blob "+digest)
		}
		lastErr = statusError(resp, "fetch blob "+digest)
		closeBody(resp)
	}
	return nil, fmt.Errorf("image: fetch blob %s after %d attempts: %w",
		digest, blobAttempts, lastErr)
}

const blobAttempts = 5

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * 250 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// resumingBlob reads a blob, reconnecting from where it stopped if the transfer
// breaks. Without this a reset partway through a large layer fails the whole
// conversion, and the layers that matter are exactly the large ones.
type resumingBlob struct {
	reg    *Registry
	ctx    context.Context
	ref    Reference
	digest string

	body io.ReadCloser
	read int64
	// stalled counts consecutive reopens that delivered nothing. Progress
	// resets it: a transfer that keeps advancing is succeeding, however many
	// times the connection drops, whereas one that reconnects repeatedly
	// without moving is stuck and should fail rather than loop.
	stalled int
}

func (b *resumingBlob) Read(p []byte) (int, error) {
	for {
		n, err := b.body.Read(p)
		b.read += int64(n)
		if n > 0 {
			// Any delivered byte means the transfer is moving, so the stall
			// budget starts over. A blob that needs many reconnections but keeps
			// advancing is succeeding, just slowly.
			b.stalled = 0
		}
		if err == nil || err == io.EOF {
			return n, err
		}
		// Bytes already read are kept; only the tail is refetched.
		if n > 0 {
			return n, nil
		}
		if b.stalled >= blobAttempts || b.ctx.Err() != nil {
			return 0, err
		}

		b.stalled++
		b.body.Close()
		next, rerr := b.reg.openBlob(b.ctx, b.ref, b.digest, b.read)
		if rerr != nil {
			return 0, fmt.Errorf("image: resume blob %s at %d: %w", b.digest, b.read, rerr)
		}
		b.body = next
	}
}

func (b *resumingBlob) Close() error { return b.body.Close() }

// FetchConfig reads an image's configuration blob.
func (r *Registry) FetchConfig(ctx context.Context, ref Reference, m *Manifest) (*Config, error) {
	body, err := r.FetchBlob(ctx, ref, m.Config.Digest)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var wrapper struct {
		Config Config `json:"config"`
	}
	raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("image: parse config: %w", err)
	}
	return &wrapper.Config, nil
}

func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func statusError(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("image: %s: HTTP %d: %s", what, resp.StatusCode,
		strings.TrimSpace(string(body)))
}
