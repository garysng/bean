package image

import (
	"errors"
	"testing"

	"github.com/garysng/bean/internal/control/store"
)

func TestZeroPolicyPermitsEverything(t *testing.T) {
	var p Policy
	if p.Enabled() {
		t.Error("zero policy reports enabled")
	}
	// The default has to be unchanged behaviour: an operator who configures
	// nothing must not start seeing refusals.
	for _, ref := range []string{"python:3.12", "ghcr.io/x/y:1", "internal.reg/app:v1"} {
		if err := p.Check(ref, nil); err != nil {
			t.Errorf("Check(%q) = %v, want nil", ref, err)
		}
	}
}

func TestPolicyOnlyBuiltRejectsUnknownRef(t *testing.T) {
	p := Policy{AllowedSources: []store.ImageSource{store.ImageBuilt}}
	// A ref the platform has never seen is about to become an import, so it
	// must be judged as one. Treating it as unknown-and-therefore-fine would
	// let any caller past the policy by naming a fresh ref.
	err := p.Check("python:3.12", nil)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
}

func TestPolicyOnlyBuiltAllowsPlatformImage(t *testing.T) {
	p := Policy{AllowedSources: []store.ImageSource{store.ImageBuilt}}
	built := &store.Image{Ref: "team/app:v1", Source: store.ImageBuilt}
	if err := p.Check("team/app:v1", built); err != nil {
		t.Errorf("built image refused: %v", err)
	}
	imported := &store.Image{Ref: "python:3.12", Source: store.ImageImported}
	if err := p.Check("python:3.12", imported); !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("imported image allowed: %v", err)
	}
}

func TestPolicyRegistryAllowlist(t *testing.T) {
	p := Policy{AllowedRegistries: []string{"registry.example.com", "index.docker.io"}}
	for _, ref := range []string{
		"registry.example.com/team/app:v1",
		"python:3.12",         // implicit Docker Hub
		"library/python:3.12", // namespace, not a host
	} {
		if err := p.Check(ref, nil); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", ref, err)
		}
	}
	if err := p.Check("ghcr.io/owner/repo:tag", nil); !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("ghcr.io allowed under allowlist: %v", err)
	}
}

func TestPolicyRegistryAllowlistSkipsBuiltImages(t *testing.T) {
	// A built image's host is a push destination the operator configured, not
	// somewhere a caller chose to pull from, so the allowlist does not apply.
	// Otherwise every deployment setting an allowlist would also have to list
	// its own registry to keep running its own builds.
	p := Policy{AllowedRegistries: []string{"registry.example.com"}}
	built := &store.Image{Ref: "ghcr.io/us/built:v1", Source: store.ImageBuilt}
	if err := p.Check("ghcr.io/us/built:v1", built); err != nil {
		t.Errorf("built image refused by registry allowlist: %v", err)
	}
}

func TestPolicyRegistryMatchIsCaseInsensitive(t *testing.T) {
	p := Policy{AllowedRegistries: []string{"Registry.Example.COM"}}
	if err := p.Check("registry.example.com/app:v1", nil); err != nil {
		t.Errorf("case mismatch refused: %v", err)
	}
}

func TestPolicyBothAxesApply(t *testing.T) {
	p := Policy{
		AllowedSources:    []store.ImageSource{store.ImageImported},
		AllowedRegistries: []string{"registry.example.com"},
	}
	if err := p.Check("registry.example.com/app:v1", nil); err != nil {
		t.Errorf("permitted ref refused: %v", err)
	}
	// Right source, wrong registry.
	if err := p.Check("ghcr.io/app:v1", nil); !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("wrong registry allowed: %v", err)
	}
	// Right registry, wrong source.
	built := &store.Image{Ref: "registry.example.com/b:v1", Source: store.ImageBuilt}
	if err := p.Check("registry.example.com/b:v1", built); !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("wrong source allowed: %v", err)
	}
}

func TestParsePolicyEmptyIsPermissive(t *testing.T) {
	p, err := ParsePolicy("", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled() {
		t.Error("empty configuration produced an enforcing policy")
	}
}

func TestParsePolicyLists(t *testing.T) {
	p, err := ParsePolicy(" built , imported ", "a.io, b.io")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.AllowedSources) != 2 {
		t.Errorf("sources = %v", p.AllowedSources)
	}
	if len(p.AllowedRegistries) != 2 || p.AllowedRegistries[0] != "a.io" {
		t.Errorf("registries = %v", p.AllowedRegistries)
	}
}

func TestParsePolicyRejectsUnknownSource(t *testing.T) {
	// Silently ignoring a typo would either allow everything or nothing, and
	// both are worse than refusing to start.
	if _, err := ParsePolicy("builtin", ""); err == nil {
		t.Error("expected an error for an unknown source")
	}
}

func TestRegistryHost(t *testing.T) {
	for ref, want := range map[string]string{
		"python:3.12":                      "index.docker.io",
		"library/python:3.12":              "index.docker.io",
		"registry.example.com/team/app:v1": "registry.example.com",
		"localhost:5000/app:v1":            "localhost:5000",
		"localhost/app":                    "localhost",
		"ghcr.io/owner/repo:tag":           "ghcr.io",
	} {
		if got := RegistryHost(ref); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", ref, got, want)
		}
	}
}
