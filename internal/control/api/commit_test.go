package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/garysng/bean/internal/control/store"
)

// The test nodes run LocalRuntime, which cannot commit, so these cover the
// control-plane half: validation, immutability, and what gets recorded. The
// node-side commit is exercised against a real microVM.

// TestCommitRejectsExistingTagWithoutDamagingIt is the guard that keeps a
// refused commit from breaking a working image. Letting the node refuse instead
// would mark the existing image FAILED on the way out — an image other sandboxes
// may already be running from.
func TestCommitRejectsExistingTagWithoutDamagingIt(t *testing.T) {
	env := startEnv(t, envOpts{})
	id := env.sandboxID(nil)

	// Stand in for an earlier successful commit.
	if err := env.Store.PutImage(&store.Image{
		Ref: "myteam/img:v1", Source: store.ImageBuilt, State: store.ImageReady,
	}); err != nil {
		t.Fatal(err)
	}

	resp, out := env.do("POST", "/v1/sandboxes/"+id+"/commit",
		map[string]any{"tag": "myteam/img:v1"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("commit over an existing tag = %d, want 409: %v", resp.StatusCode, out)
	}
	apiErr, _ := out["error"].(map[string]any)
	if msg, _ := apiErr["message"].(string); !strings.Contains(msg, "immutable") {
		t.Errorf("error should explain immutability, got %q", msg)
	}

	img, err := env.Store.GetImage("myteam/img:v1")
	if err != nil || img == nil {
		t.Fatalf("image disappeared: %v", err)
	}
	if img.State != store.ImageReady {
		t.Errorf("refused commit changed the existing image to %s", img.State)
	}
}

func TestCommitValidatesTag(t *testing.T) {
	env := startEnv(t, envOpts{})
	id := env.sandboxID(nil)

	for name, tag := range map[string]any{
		"missing":    nil,
		"empty":      "",
		"whitespace": "has space",
		"leading -":  "-bad",
	} {
		body := map[string]any{}
		if tag != nil {
			body["tag"] = tag
		}
		resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/commit", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s tag: got %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestCommitUnknownSandbox(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("POST", "/v1/sandboxes/sbx_absent/commit",
		map[string]any{"tag": "myteam/img:v1"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("commit on a missing sandbox = %d, want 404", resp.StatusCode)
	}
}

// TestCommitMarksImageFailedWhenNodeRefuses checks the caller is not left
// waiting on an image that will never become ready.
func TestCommitMarksImageFailedWhenNodeRefuses(t *testing.T) {
	env := startEnv(t, envOpts{})
	id := env.sandboxID(nil)

	// LocalRuntime cannot commit, so the node returns an error — which is the
	// path being tested.
	resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/commit",
		map[string]any{"tag": "myteam/willfail:v1"})
	if resp.StatusCode < 400 {
		t.Fatalf("commit on a runtime that cannot commit = %d, want an error", resp.StatusCode)
	}

	img, err := env.Store.GetImage("myteam/willfail:v1")
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Fatal("no image recorded; a caller polling would see nothing")
	}
	if img.State != store.ImageFailed {
		t.Errorf("image state = %s, want FAILED", img.State)
	}
}
