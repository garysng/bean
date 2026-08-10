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

// ErrNotFound reports that an image reference is not registered.
var ErrNotFound = errors.New("image not found")

// ErrForbidden reports that an image belongs to another identity, so the caller
// may not act on it.
var ErrForbidden = errors.New("image belongs to another owner")

// NodeCacheSource reports how many nodes have an image cached, which drives
// prewarm progress and image-affinity scoring.
type NodeCacheSource interface {
	CachedNodeCount(ref string) int
}

// Service owns image metadata and prewarm jobs.
// Store is what the image service needs: the image catalogue and the prewarm jobs
// that populate it. Snapshots, sandboxes and the resource ledger are unreachable
// through it, which is the point -- an image service that could move a reservation
// would be a second scheduler.
type Store interface {
	store.Templates
}

type Service struct {
	store  Store
	cache  NodeCacheSource
	policy Policy

	mu sync.Mutex
}

func New(st Store, cache NodeCacheSource) *Service {
	return &Service{store: st, cache: cache}
}

// NewWithPolicy builds a service that refuses references an operator's policy
// forbids. The zero Policy permits everything, so this is New plus a rule.
func NewWithPolicy(st Store, cache NodeCacheSource, policy Policy) *Service {
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
func (s *Service) Resolve(ref string) (*store.Template, error) {
	return s.ResolveFor(ref, "")
}

// ResolveFor is Resolve on behalf of an identity: it applies the operator's
// policy and attributes a first-seen reference to owner.
//
// A converted OCI image is a template whose name is the OCI reference, so an
// unseen ref registers a new PENDING template named by the ref; a ref already
// converted resolves to its existing template. Ownership is claimed only on
// first registration and never reassigned. A shared base image would otherwise
// change hands with every caller that ran it, and the last one to touch it is
// not a useful answer to "whose is this". An empty owner leaves the template
// unowned, which is what a deployment with no identity source produces.
func (s *Service) ResolveFor(ref, owner string) (*store.Template, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tpl, err := s.store.GetTemplateByName(ref)
	if err != nil {
		return nil, err
	}
	// The policy is checked against the record as it stands, including for a
	// reference already registered: an operator who tightens the policy means
	// it to apply to the next create, not only to refs nobody has run yet.
	if s.policy.Enabled() {
		if err := s.policy.Check(ref, tpl); err != nil {
			return nil, err
		}
	}
	if tpl != nil {
		return s.withCacheCount(tpl), nil
	}
	now := time.Now()
	tpl = &store.Template{
		ID:        store.NewID(store.PrefixTemplate),
		Name:      ref,
		OCISource: &store.OCISource{Ref: ref},
		State:     store.TemplatePending,
		Source:    store.TemplateConverted,
		Owner:     owner,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.PutTemplate(tpl); err != nil {
		return nil, err
	}
	return s.withCacheCount(tpl), nil
}

// Get returns template metadata by its OCI ref (its name), or nil if the
// reference was never used.
func (s *Service) Get(ref string) (*store.Template, error) {
	tpl, err := s.store.GetTemplateByName(ref)
	if err != nil || tpl == nil {
		return nil, err
	}
	return s.withCacheCount(tpl), nil
}

// List returns every known template, most recently updated first. It is the
// operator's view; a per-caller listing goes through ListFor.
func (s *Service) List() ([]*store.Template, error) {
	return s.ListFor("")
}

// ListFor returns the templates an identity may see: its own plus the unowned
// ones. An empty owner returns everything, which is both the operator's view
// and what a deployment with no identity source can answer.
func (s *Service) ListFor(owner string) ([]*store.Template, error) {
	tpls, err := s.store.ListTemplates(owner)
	if err != nil {
		return nil, err
	}
	for _, tpl := range tpls {
		s.withCacheCount(tpl)
	}
	return tpls, nil
}

// Delete removes a template record. It is the operator's unscoped delete; a
// per-caller delete goes through DeleteFor.
func (s *Service) Delete(ref string) error {
	return s.DeleteFor(ref, "")
}

// DeleteFor removes a template an identity is allowed to remove: its own, or an
// unowned one. It returns ErrNotFound when the reference is unknown, and
// ErrForbidden when it belongs to another identity, so a caller cannot use delete
// to probe for the existence of templates it may not see.
//
// The scope mirrors ListFor: an empty owner is the operator's view and may delete
// anything, an identity may delete what it owns and the unowned templates every
// identity shares. Deletion removes only the control-plane record; published layers
// are content-addressed and shared, so they are reclaimed by garbage collection, not
// by deleting one template that referenced them.
func (s *Service) DeleteFor(ref, owner string) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tpl, err := s.store.GetTemplateByName(ref)
	if err != nil {
		return err
	}
	if tpl == nil {
		return ErrNotFound
	}
	if owner != "" && tpl.Owner != "" && tpl.Owner != owner {
		return ErrForbidden
	}
	return s.store.DeleteTemplate(tpl.ID)
}

// MarkConverting records that a conversion has started.
func (s *Service) MarkConverting(ref string) error {
	return s.transition(ref, store.TemplateConverting, "", func(tpl *store.Template) {})
}

// MarkReady records a successful conversion along with the artifact digest, size
// and layer chain, making the template usable by the fc tier.
//
// layerDigests is the published layer chain, base first. It is recorded so per-layer
// dedup and cache accounting have it without a second round trip; a build that stayed
// node-local publishes nothing and passes an empty digest, zero size and no digests.
//
// ociDigest is the OCI content sha256 an OCI conversion resolved node-side; it
// completes the OCISource{ref, digest} conversion-cache key so a later create with
// the same ref reuses this template without re-converting. A built template has no
// OCI origin and passes an empty ociDigest, leaving OCISource nil.
//
// cfg is the image configuration the node recovered -- the ENV/ENTRYPOINT/CMD/WORKDIR
// a build declared or a converted image carries -- recorded on the template so a create
// honours it and `template status` shows it. Nil when the artifact declared none.
func (s *Service) MarkReady(ref, fsDigest string, sizeBytes int64, layerDigests []string, ociDigest string, cfg *store.Config) error {
	return s.transition(ref, store.TemplateReady, "", func(tpl *store.Template) {
		tpl.FS.Digest = fsDigest
		tpl.FS.SizeBytes = sizeBytes
		tpl.FS.LayerDigests = layerDigests
		tpl.FS.Config = cfg
		if ociDigest != "" {
			if tpl.OCISource == nil {
				tpl.OCISource = &store.OCISource{Ref: ref}
			}
			tpl.OCISource.Digest = ociDigest
		}
	})
}

// MarkFailed records a conversion failure with its reason.
func (s *Service) MarkFailed(ref, reason string) error {
	return s.transition(ref, store.TemplateFailed, reason, func(tpl *store.Template) {})
}

func (s *Service) transition(ref string, to store.TemplateState, reason string,
	mutate func(*store.Template)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tpl, err := s.store.GetTemplateByName(ref)
	if err != nil {
		return err
	}
	if tpl == nil {
		return fmt.Errorf("template %s not registered", ref)
	}
	tpl.State = to
	tpl.Reason = reason
	mutate(tpl)
	return s.store.PutTemplate(tpl)
}

// withCacheCount fills in the live node-cache count, which is not persisted
// because it changes with every heartbeat.
func (s *Service) withCacheCount(tpl *store.Template) *store.Template {
	if s.cache != nil && tpl.OCISource != nil {
		tpl.CachedNodes = s.cache.CachedNodeCount(tpl.OCISource.Ref)
	}
	return tpl
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
