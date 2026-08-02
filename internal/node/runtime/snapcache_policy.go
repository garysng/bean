package runtime

import "fmt"

// EvictionPolicy bounds the unpacked snapshot cache.
//
// The watermark pair is borrowed from kubelet's image garbage collection:
// reclaiming at a high mark down to a low one, rather than at a single threshold,
// is what keeps eviction an occasional batch instead of something every
// subsequent restore pays for.
//
// Declared outside the platform-specific files so noded parses the same flags
// everywhere and reports an unsupported platform as an error rather than a build
// failure — the same reason FCTierConfig lives here.
type EvictionPolicy struct {
	// HighBytes triggers a sweep once the cache is at least this large. Zero
	// disables eviction, which is what a node with a dedicated cache volume
	// wants.
	HighBytes int64
	// LowBytes is the size a sweep reclaims down to. It must be below HighBytes:
	// reclaiming only to the trigger point would leave the cache one entry away
	// from sweeping again on the next restore.
	LowBytes int64
}

// DefaultEvictionPolicy leaves eviction off. A default measured in bytes would be
// a guess about the node's disk, and guessing low evicts entries a live fan-out is
// about to reuse — a silent performance regression rather than an error, so the
// operator opts in.
func DefaultEvictionPolicy() EvictionPolicy { return EvictionPolicy{} }

// Validate rejects a policy whose marks cannot bound anything.
func (p EvictionPolicy) Validate() error {
	if p.HighBytes == 0 && p.LowBytes == 0 {
		return nil
	}
	if p.HighBytes <= 0 {
		return fmt.Errorf("snapshot cache high watermark must be positive, got %d", p.HighBytes)
	}
	if p.LowBytes <= 0 {
		return fmt.Errorf("snapshot cache low watermark must be positive, got %d", p.LowBytes)
	}
	if p.LowBytes >= p.HighBytes {
		return fmt.Errorf("snapshot cache low watermark (%d) must be below the high "+
			"watermark (%d), or every restore past the trigger would evict",
			p.LowBytes, p.HighBytes)
	}
	return nil
}

// Enabled reports whether a sweep can run.
func (p EvictionPolicy) Enabled() bool { return p.HighBytes > 0 && p.LowBytes > 0 }
