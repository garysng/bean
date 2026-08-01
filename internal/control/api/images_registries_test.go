package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestImageStatusUnknownRef(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("GET", "/v1/images/status?ref=never%3Aseen", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "IMAGE_NOT_FOUND" {
		t.Errorf("code = %v", code)
	}
}

func TestImageStatusRequiresRef(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("GET", "/v1/images/status", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPrewarmRegistersImages(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("POST", "/v1/images/prewarm", map[string]any{
		"refs":        []string{"python:3.12", "registry.example.com/team/app:v1"},
		"targetNodes": 2,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	jobID := out["jobId"].(string)
	if !strings.HasPrefix(jobID, "pw_") {
		t.Errorf("jobId = %q, want pw_ prefix", jobID)
	}

	// The referenced images are now known, with the OCI format because
	// nothing has been converted.
	_, out = env.do("GET", "/v1/images/status?ref="+url.QueryEscape("python:3.12"), nil)
	if out["state"] != "PENDING" {
		t.Errorf("state = %v, want PENDING", out["state"])
	}
	if out["format"] != "oci" {
		t.Errorf("format = %v, want oci", out["format"])
	}

	_, out = env.do("GET", "/v1/images", nil)
	if n := len(out["images"].([]any)); n != 2 {
		t.Errorf("images = %d, want 2", n)
	}

	// Job status reflects readiness (no nodes report cache in this stack).
	_, out = env.do("GET", "/v1/images/prewarm/"+jobID, nil)
	if out["jobId"] != jobID {
		t.Errorf("jobId = %v", out["jobId"])
	}
}

func TestPrewarmValidationViaAPI(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("POST", "/v1/images/prewarm", map[string]any{"refs": []string{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	resp, _ = env.do("POST", "/v1/images/prewarm", map[string]any{"refs": []string{"bad ref"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad ref status = %d, want 400", resp.StatusCode)
	}
}

func TestPrewarmJobNotFound(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("GET", "/v1/images/prewarm/pw_missing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRegistryCredentialLifecycle(t *testing.T) {
	env := startEnv(t, envOpts{WithSecrets: true})

	resp, out := env.do("PUT", "/v1/registries", map[string]any{
		"host": "https://registry.example.com/", "username": "robot",
		"secret": "super-secret-token",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	// The scheme and trailing slash are normalised away.
	if out["host"] != "registry.example.com" {
		t.Errorf("host = %v, want normalised", out["host"])
	}
	// The response must never echo the secret.
	if _, leaked := out["secret"]; leaked {
		t.Error("secret echoed in response")
	}

	resp, out = env.do("GET", "/v1/registries", nil)
	regs := out["registries"].([]any)
	if len(regs) != 1 {
		t.Fatalf("registries = %v", regs)
	}
	first := regs[0].(map[string]any)
	if first["username"] != "robot" {
		t.Errorf("username = %v", first["username"])
	}
	if _, leaked := first["secret"]; leaked {
		t.Error("secret leaked in list response")
	}

	resp, _ = env.do("DELETE", "/v1/registries/registry.example.com", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d", resp.StatusCode)
	}
	resp, _ = env.do("DELETE", "/v1/registries/registry.example.com", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestRegistryCredentialValidation(t *testing.T) {
	env := startEnv(t, envOpts{WithSecrets: true})
	resp, _ := env.do("PUT", "/v1/registries", map[string]any{"username": "u", "secret": "s"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing host status = %d", resp.StatusCode)
	}
	resp, _ = env.do("PUT", "/v1/registries", map[string]any{"host": "h"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing secret status = %d", resp.StatusCode)
	}
}

func TestRegistryRequiresMasterKey(t *testing.T) {
	// Without a key the endpoint refuses rather than storing plaintext.
	env := startEnv(t, envOpts{})
	resp, out := env.do("PUT", "/v1/registries", map[string]any{
		"host": "r.example.com", "secret": "s",
	})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %v", resp.StatusCode, out)
	}
}

func TestRegistryHostOf(t *testing.T) {
	cases := map[string]string{
		"python:3.12":                      "index.docker.io",
		"library/python:3.12":              "index.docker.io",
		"registry.example.com/team/app:v1": "registry.example.com",
		"localhost:5000/app:v1":            "localhost:5000",
		"localhost/app":                    "localhost",
		"ghcr.io/owner/repo:tag":           "ghcr.io",
	}
	for ref, want := range cases {
		if got := RegistryHostOf(ref); got != want {
			t.Errorf("RegistryHostOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://r.io/": "r.io",
		"http://r.io":   "r.io",
		"  r.io  ":      "r.io",
		"r.io":          "r.io",
	} {
		if got := normalizeRegistryHost(in); got != want {
			t.Errorf("normalizeRegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageEndpointsDisabledWithoutService(t *testing.T) {
	env := startEnv(t, envOpts{WithoutImages: true})
	for _, path := range []string{"/v1/images", "/v1/images/status?ref=x"} {
		resp, _ := env.do("GET", path, nil)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, resp.StatusCode)
		}
	}
}
