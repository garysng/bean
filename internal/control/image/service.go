// Package image manages platform-side image metadata: it registers the
// native OCI references callers supply, tracks conversion state, and
// orchestrates prewarming. It is a control-plane module (like scheduler),
// not a separate service — see docs/architecture.md D4.
//
// Terminology, because it is easy to conflate:
//
//   - ref: the native OCI reference the caller wrote ("python:3.12"). This
//     is the only image identifier a caller ever supplies or sees.
//   - digest: the resolved content hash. Scheduling, caching and
//     reproducibility key off the digest so a moving tag cannot change what
//     a batch runs.
//   - overlaybd artifact: the converted block-device form used by the fc
//     tier. Entirely internal; conversion is invisible to callers.
package image

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

// ErrInvalidRef reports a malformed image reference.
var ErrInvalidRef = errors.New("invalid image reference")

// NodeCacheSource reports how many nodes have an image cached, which drives
// prewarm progress and image-affinity scoring.
type NodeCacheSource interface {
	CachedNodeCount(ref string) int
}

// Service owns image metadata and prewarm jobs.
type Service struct {
	store  *store.Store
	cache  NodeCacheSource
	policy Policy

	mu sync.Mutex
}

func New(st *store.Store, cache NodeCacheSource) *Service {
	return &Service{store: st, cache: cache}
}

// NewWithPolicy builds a service that refuses references an operator's policy
// forbids. The zero Policy permits everything, so this is New plus a rule.
func NewWithPolicy(st *store.Store, cache NodeCacheSource, policy Policy) *Service {
	return &Service{store: st, cache: cache, policy: policy}
}

// Policy returns the configured policy, so a handler can report what it is
// without holding a second copy that could drift.
func (s *Service) Policy() Policy { return s.policy }

// Resolve registers a reference if unseen and returns its metadata. It is
// the entry point every sandbox create goes through, so an image record
// always exists for anything the platform has been asked to run.
//
// Digest resolution and overlaybd conversion are deliberately not done
// here: they need a registry client and a converter, which arrive with the
// fc tier. Until then an image stays PENDING, which the container/local
// tiers can still run because they pull through the standard path.
func (s *Service) Resolve(ref string) (*store.Image, error) {
	return s.ResolveFor(ref, "")
}

// ResolveFor is Resolve on behalf of an identity: it applies the operator's
// policy and attributes a first-seen reference to owner.
//
// Ownership is claimed only on first registration and never reassigned. A
// shared base image would otherwise change hands with every caller that ran
// it, and the last one to touch it is not a useful answer to "whose is this".
// An empty owner leaves the image unowned, which is what a deployment with no
// identity source produces and what every pre-existing image already is.
func (s *Service) ResolveFor(ref, owner string) (*store.Image, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	img, err := s.store.GetImage(ref)
	if err != nil {
		return nil, err
	}
	// The policy is checked against the record as it stands, including for a
	// reference already registered: an operator who tightens the policy means
	// it to apply to the next create, not only to refs nobody has run yet.
	if s.policy.Enabled() {
		if err := s.policy.Check(ref, img); err != nil {
			return nil, err
		}
	}
	if img != nil {
		return s.withCacheCount(img), nil
	}
	now := time.Now()
	img = &store.Image{
		Ref:       ref,
		State:     store.ImagePending,
		Source:    store.ImageImported,
		Owner:     owner,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.PutImage(img); err != nil {
		return nil, err
	}
	return s.withCacheCount(img), nil
}

// Get returns image metadata, or nil if the reference was never used.
func (s *Service) Get(ref string) (*store.Image, error) {
	img, err := s.store.GetImage(ref)
	if err != nil || img == nil {
		return nil, err
	}
	return s.withCacheCount(img), nil
}

// List returns every known image, most recently updated first. It is the
// operator's view; a per-caller listing goes through ListFor.
func (s *Service) List() ([]*store.Image, error) {
	return s.ListFor("")
}

// ListFor returns the images an identity may see: its own plus the unowned
// ones. An empty owner returns everything, which is both the operator's view
// and what a deployment with no identity source can answer.
func (s *Service) ListFor(owner string) ([]*store.Image, error) {
	imgs, err := s.store.ListImages(owner)
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		s.withCacheCount(img)
	}
	return imgs, nil
}

// MarkConverting records that a conversion has started.
func (s *Service) MarkConverting(ref string) error {
	return s.transition(ref, store.ImageConverting, "", func(img *store.Image) {})
}

// MarkReady records a successful conversion along with the artifact ref and
// size, making the image usable by the fc tier.
func (s *Service) MarkReady(ref, overlaybdRef string, sizeBytes int64) error {
	return s.transition(ref, store.ImageReady, "", func(img *store.Image) {
		img.OverlaybdRef = overlaybdRef
		img.SizeBytes = sizeBytes
	})
}

// MarkFailed records a conversion failure with its reason.
func (s *Service) MarkFailed(ref, reason string) error {
	return s.transition(ref, store.ImageFailed, reason, func(img *store.Image) {})
}

func (s *Service) transition(ref string, to store.ImageState, reason string,
	mutate func(*store.Image)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.store.GetImage(ref)
	if err != nil {
		return err
	}
	if img == nil {
		return fmt.Errorf("image %s not registered", ref)
	}
	img.State = to
	img.Reason = reason
	mutate(img)
	return s.store.PutImage(img)
}

// withCacheCount fills in the live node-cache count, which is not persisted
// because it changes with every heartbeat.
func (s *Service) withCacheCount(img *store.Image) *store.Image {
	if s.cache != nil {
		img.CachedNodes = s.cache.CachedNodeCount(img.Ref)
	}
	return img
}

// PrewarmRequest asks the platform to pull images onto nodes ahead of a
// batch so the batch does not pay the cold-pull cost.
type PrewarmRequest struct {
	Refs        []string
	Region      string
	TargetNodes int
	Priority    string
}

// Prewarm registers the images and records a job. Node-side execution
// lands with the overlaybd pipeline; until then the job reports current
// cache counts, which is already the useful signal for a caller deciding
// whether to start a batch.
func (s *Service) Prewarm(req PrewarmRequest) (*store.PrewarmJob, error) {
	if len(req.Refs) == 0 {
		return nil, fmt.Errorf("%w: at least one ref required", ErrInvalidRef)
	}
	for _, ref := range req.Refs {
		if err := ValidateRef(ref); err != nil {
			return nil, err
		}
	}
	// Registering up front means a later status query has metadata to report.
	for _, ref := range req.Refs {
		if _, err := s.Resolve(ref); err != nil {
			return nil, err
		}
	}
	priority := req.Priority
	if priority == "" {
		priority = "normal"
	}
	job := &store.PrewarmJob{
		ID:          store.NewID(store.PrefixPrewarmJob),
		Refs:        req.Refs,
		Region:      req.Region,
		TargetNodes: req.TargetNodes,
		Priority:    priority,
		CreatedAt:   time.Now(),
	}
	if err := s.store.PutPrewarmJob(job); err != nil {
		return nil, err
	}
	return s.JobStatus(job.ID)
}

// JobStatus returns the job with a live image-by-node readiness matrix.
func (s *Service) JobStatus(jobID string) (*store.PrewarmJob, error) {
	job, err := s.store.GetPrewarmJob(jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}
	job.Ready = make(map[string]int, len(job.Refs))
	done := true
	for _, ref := range job.Refs {
		n := 0
		if s.cache != nil {
			n = s.cache.CachedNodeCount(ref)
		}
		job.Ready[ref] = n
		if job.TargetNodes > 0 && n < job.TargetNodes {
			done = false
		}
	}
	job.Done = done
	return job, nil
}

// ValidateRef rejects references that cannot name an image. It is
// deliberately permissive about registry syntax — the registry is the
// authority — but catches the mistakes that would otherwise surface as a
// confusing failure much later.
func ValidateRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("%w: empty", ErrInvalidRef)
	case strings.ContainsAny(ref, " \t\n"):
		return fmt.Errorf("%w: contains whitespace", ErrInvalidRef)
	case strings.HasPrefix(ref, "-"):
		return fmt.Errorf("%w: leading dash", ErrInvalidRef)
	case len(ref) > 512:
		return fmt.Errorf("%w: too long", ErrInvalidRef)
	}
	return nil
}
