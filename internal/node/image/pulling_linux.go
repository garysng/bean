//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// PullingProvider converts an image on first use, so a node does not need its
// images staged out of band.
//
// It wraps another provider rather than replacing one: assembling a rootfs and
// obtaining the image it is assembled from are separate concerns, and the
// copy-on-write assembly is worth having regardless of where the base came from.
//
// Conversions are deduplicated. Placing a batch of sandboxes on the same image
// is the normal case for evaluation, and without this each one would pull and
// convert the same layers concurrently — wasting bandwidth and, because they
// publish to the same path, racing each other.
type PullingProvider struct {
	// Inner assembles the rootfs once the base image is present.
	Inner Provider
	// Converter pulls and converts a missing image.
	Converter *Converter

	mu       sync.Mutex
	inflight map[string]*pullResult
}

// pullResult lets waiters share one conversion's outcome.
type pullResult struct {
	done chan struct{}
	err  error
}

func NewPullingProvider(inner Provider, converter *Converter) *PullingProvider {
	return &PullingProvider{
		Inner:     inner,
		Converter: converter,
		inflight:  map[string]*pullResult{},
	}
}

func (p *PullingProvider) Name() string { return p.Inner.Name() + "+pull" }

// Cached defers to the inner provider, which owns the image directory.
func (p *PullingProvider) Cached() (map[string]int64, error) { return p.Inner.Cached() }

func (p *PullingProvider) Prepare(ctx context.Context, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error) {
	rootfs, err := p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
	if !errors.Is(err, ErrNotCached) {
		return rootfs, err
	}
	if err := p.ensure(ctx, imageRef); err != nil {
		return nil, err
	}
	return p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
}

// Prewarm converts an image without creating a sandbox, which is the node side
// of the control plane's prewarm job.
func (p *PullingProvider) Prewarm(ctx context.Context, imageRef string) error {
	if err := p.Inner.Prewarm(ctx, imageRef); !errors.Is(err, ErrNotCached) {
		return err
	}
	return p.ensure(ctx, imageRef)
}

// ensure converts an image, collapsing concurrent requests for the same
// reference into one conversion.
func (p *PullingProvider) ensure(ctx context.Context, imageRef string) error {
	p.mu.Lock()
	if existing, ok := p.inflight[imageRef]; ok {
		p.mu.Unlock()
		// Waiting shares the outcome, but the caller's own cancellation still
		// applies: a client that gave up should not be held by someone else's
		// pull.
		select {
		case <-existing.done:
			return existing.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	result := &pullResult{done: make(chan struct{})}
	p.inflight[imageRef] = result
	p.mu.Unlock()

	// The conversion deliberately does not inherit the caller's context: other
	// waiters depend on it, so one client cancelling must not abandon the work
	// the rest are waiting for.
	_, err := p.Converter.Convert(context.WithoutCancel(ctx), imageRef)
	if err != nil {
		result.err = fmt.Errorf("image: convert %s: %w", imageRef, err)
	}
	close(result.done)

	p.mu.Lock()
	delete(p.inflight, imageRef)
	p.mu.Unlock()

	return result.err
}
