package api

import (
	"net/http"
	"strings"
)

// Identity attribution for image ownership.
//
// Authentication here is a single shared API key, so this layer cannot tell
// two callers apart on its own and does not try to. An external platform layer
// is expected to front this API and do real authentication; what it needs from
// us is somewhere to say who a request is for, and for that answer to be used
// consistently.
//
// The assumption, stated plainly because everything below depends on it: the
// fronting layer is trusted, and the header it sets is trusted exactly as far
// as that layer is. Bean does not verify it and must not be exposed directly to
// untrusted clients with an identity header enabled — a caller could then
// simply name someone else. Enforcement is that layer's job; ours is to record
// attribution and scope listings by it.
//
// This is a function rather than a header name so the pluggable part is the
// whole extraction: a later JWT claim, mTLS subject or session lookup replaces
// OwnerFromHeader without touching any handler.

// OwnerHeader is the default header carrying the caller's identity.
const OwnerHeader = "X-Bean-Owner"

// IdentityFunc derives the owner an image should be attributed to. Returning
// an empty string means unowned, which keeps a deployment that configures no
// identity behaving exactly as it did before ownership existed.
type IdentityFunc func(*http.Request) string

// OwnerFromHeader reads the identity from a header, trimming whitespace. It is
// the default because a reverse proxy or gateway can set a header without
// speaking any bean-specific protocol.
func OwnerFromHeader(header string) IdentityFunc {
	if header == "" {
		header = OwnerHeader
	}
	return func(r *http.Request) string {
		return strings.TrimSpace(r.Header.Get(header))
	}
}

// owner resolves the caller's identity, or "" when none is configured.
func (s *Server) owner(r *http.Request) string {
	if s.identity == nil {
		return ""
	}
	return s.identity(r)
}
