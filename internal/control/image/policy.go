package image

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/garysng/bean/internal/control/store"
)

// ErrPolicyDenied reports a reference an operator's policy forbids. It is a
// decision about the request, not a failure of the platform, so callers must
// translate it into a client error rather than an internal one.
var ErrPolicyDenied = errors.New("image not permitted by policy")

// Policy decides which images a sandbox may be started from.
//
// The zero Policy permits everything, which is what a deployment that
// configures nothing gets: the platform pulled whatever ref it was handed
// before this existed, and quietly starting to refuse would break running
// deployments to no one's benefit.
//
// The two axes are deliberately separate because they answer different
// questions. AllowedSources answers "did this platform produce the image",
// which is about provenance and cannot be spoofed by naming a registry.
// AllowedRegistries answers "may we pull from there at all", which is about
// egress and applies even to images this platform built and pushed.
type Policy struct {
	// AllowedSources restricts template provenance, e.g. only TemplateBuilt to
	// keep a deployment to templates it produced itself. Empty allows any source.
	//
	// A reference the platform has never seen is treated as TemplateConverted: it
	// is about to become one, and the alternative is a policy that a caller
	// can pass by using a ref nobody registered yet.
	AllowedSources []store.TemplateSource
	// AllowedRegistries is a registry host allowlist for converted references,
	// e.g. "registry.example.com" or "index.docker.io" for Docker Hub. Empty
	// allows any registry.
	//
	// Built templates skip this check. Their name is a tag the operator chose,
	// not a place a caller chose to pull from, and including it here would mean
	// every deployment that sets an allowlist also has to remember to list its
	// own registry.
	AllowedRegistries []string
}

// Enabled reports whether the policy would refuse anything, which lets a
// caller skip a store lookup that only the policy needs.
func (p Policy) Enabled() bool {
	return len(p.AllowedSources) > 0 || len(p.AllowedRegistries) > 0
}

// Check validates a reference against the policy. The known argument is the
// existing image record, or nil when the reference has never been registered.
func (p Policy) Check(ref string, known *store.Template) error {
	source := store.TemplateConverted
	if known != nil && known.Source != "" {
		source = known.Source
	}

	if len(p.AllowedSources) > 0 && !containsSource(p.AllowedSources, source) {
		return fmt.Errorf("%w: %s is %s, and this deployment allows only %s",
			ErrPolicyDenied, ref, source, joinSources(p.AllowedSources))
	}

	// Only converted references are subject to the registry allowlist; see the
	// field comment for why a built template's host is not the caller's choice.
	if len(p.AllowedRegistries) > 0 && source == store.TemplateConverted {
		host := RegistryHost(ref)
		if !containsFold(p.AllowedRegistries, host) {
			return fmt.Errorf("%w: %s is on %s, and this deployment allows only %s",
				ErrPolicyDenied, ref, host, strings.Join(p.AllowedRegistries, ", "))
		}
	}
	return nil
}

func containsSource(list []store.TemplateSource, want store.TemplateSource) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func joinSources(list []store.TemplateSource) string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ParsePolicy builds a Policy from operator-supplied lists, as they arrive
// from flags or a config file. Empty strings yield the permissive zero
// Policy, so an operator who sets nothing keeps today's behaviour.
//
// An unrecognised source is an error rather than an ignored entry: a
// misspelled "builtin" that silently allowed nothing would take a deployment
// down, and one that silently allowed everything would be worse.
func ParsePolicy(sources, registries string) (Policy, error) {
	var p Policy
	for _, s := range splitList(sources) {
		switch store.TemplateSource(strings.ToLower(s)) {
		case store.TemplateBuilt:
			p.AllowedSources = append(p.AllowedSources, store.TemplateBuilt)
		case store.TemplateConverted:
			p.AllowedSources = append(p.AllowedSources, store.TemplateConverted)
		default:
			return Policy{}, fmt.Errorf("unknown template source %q: want %s or %s",
				s, store.TemplateBuilt, store.TemplateConverted)
		}
	}
	for _, host := range splitList(registries) {
		p.AllowedRegistries = append(p.AllowedRegistries, host)
	}
	return p, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// RegistryHost extracts the registry host from an OCI reference, defaulting
// to Docker Hub when the reference omits one — the same rule container
// runtimes apply, so "python:3.12" resolves as users expect.
func RegistryHost(ref string) string {
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	} else {
		return "index.docker.io"
	}
	// A first segment is a registry only if it looks like a host: it has a
	// dot, a port, or is localhost. Otherwise it is a Docker Hub namespace
	// ("library/python").
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "index.docker.io"
}
