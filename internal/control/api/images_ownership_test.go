package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
)

// refsOf collects the refs from a /v1/images response.
func refsOf(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	got := map[string]string{}
	list, _ := out["images"].([]any)
	for _, item := range list {
		img, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("image entry is not an object: %v", item)
		}
		ref, _ := img["ref"].(string)
		source, _ := img["source"].(string)
		got[ref] = source
	}
	return got
}

func TestCreateWithNoPolicyAllowsAnyImage(t *testing.T) {
	// The default has to be today's behaviour: an operator who sets no policy
	// sees no new refusals.
	env := startEnv(t, envOpts{})
	resp, out := env.do("POST", "/v1/sandboxes",
		map[string]any{"image": "ghcr.io/anyone/anything:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
}

func TestCreateRejectsImageOutsideRegistryAllowlist(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedRegistries: []string{"registry.example.com"}},
	})
	resp, out := env.do("POST", "/v1/sandboxes",
		map[string]any{"image": "ghcr.io/outside/app:v1"})
	// A policy refusal is not a 500 and not a malformed request: the caller is
	// asking for something this deployment does not permit.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "IMAGE_NOT_PERMITTED" {
		t.Errorf("code = %v, want IMAGE_NOT_PERMITTED", code)
	}
	// Nothing was registered, so a refused image does not pollute the listing.
	_, out = env.do("GET", "/v1/images", nil)
	if refs := refsOf(t, out); len(refs) != 0 {
		t.Errorf("refused image registered: %v", refs)
	}
}

func TestCreateAllowsImageInsideRegistryAllowlist(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedRegistries: []string{"index.docker.io"}},
	})
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "python:3.12"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", resp.StatusCode, out)
	}
}

func TestCreateRejectsImportedImageWhenOnlyBuiltAllowed(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedSources: []store.ImageSource{store.ImageBuilt}},
	})
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "python:3.12"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %v", resp.StatusCode, out)
	}
}

func TestCreateAllowsPlatformBuiltImageWhenOnlyBuiltAllowed(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedSources: []store.ImageSource{store.ImageBuilt}},
	})
	// A platform-built image is on record as built, so the same policy that
	// refuses an import lets this through.
	if err := env.Store.PutImage(&store.Image{
		Ref: "ours/app:v1", Source: store.ImageBuilt, State: store.ImageReady,
	}); err != nil {
		t.Fatal(err)
	}
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "ours/app:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", resp.StatusCode, out)
	}
}

func TestImageListScopedToCallerIdentity(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})

	// Two callers each run their own image, plus one they share.
	for _, tc := range []struct{ owner, ref string }{
		{"user-a", "a-only:v1"},
		{"user-b", "b-only:v1"},
	} {
		resp, out := env.doAs(tc.owner, "POST", "/v1/sandboxes", map[string]any{"image": tc.ref})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: %d %v", tc.ref, resp.StatusCode, out)
		}
	}

	_, out := env.doAs("user-a", "GET", "/v1/images", nil)
	refs := refsOf(t, out)
	if _, ok := refs["a-only:v1"]; !ok {
		t.Error("caller cannot see their own image")
	}
	if _, ok := refs["b-only:v1"]; ok {
		t.Error("another caller's image leaked into the listing")
	}

	// An unscoped request is the operator's view and sees both.
	_, out = env.do("GET", "/v1/images", nil)
	if refs := refsOf(t, out); len(refs) != 2 {
		t.Errorf("operator view = %v, want both images", refs)
	}
}

func TestImageListWithoutIdentityIsUnfiltered(t *testing.T) {
	// No identity configured means every image is unowned and every listing is
	// unfiltered, which is exactly the behaviour before ownership existed.
	env := startEnv(t, envOpts{})
	env.createSandbox(map[string]any{"image": "one:v1"})
	env.createSandbox(map[string]any{"image": "two:v1"})

	_, out := env.doAs("user-a", "GET", "/v1/images", nil)
	if refs := refsOf(t, out); len(refs) != 2 {
		t.Errorf("images = %v, want both despite the header", refs)
	}
}

func TestImageListFiltersBySource(t *testing.T) {
	env := startEnv(t, envOpts{})
	env.createSandbox(map[string]any{"image": "pulled:v1"})
	if err := env.Store.PutImage(&store.Image{
		Ref: "built:v1", Source: store.ImageBuilt, State: store.ImageReady,
	}); err != nil {
		t.Fatal(err)
	}

	_, out := env.do("GET", "/v1/images?source=built", nil)
	refs := refsOf(t, out)
	if len(refs) != 1 || refs["built:v1"] != string(store.ImageBuilt) {
		t.Errorf("built listing = %v", refs)
	}

	_, out = env.do("GET", "/v1/images?source=imported", nil)
	refs = refsOf(t, out)
	if len(refs) != 1 || refs["pulled:v1"] != string(store.ImageImported) {
		t.Errorf("imported listing = %v", refs)
	}

	resp, _ := env.do("GET", "/v1/images?source=nonsense", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown source status = %d, want 400", resp.StatusCode)
	}
}

func TestImageStatusReportsOriginAndOwner(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})
	resp, out := env.doAs("user-a", "POST", "/v1/sandboxes",
		map[string]any{"image": "mine:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}

	_, out = env.doAs("user-a", "GET",
		"/v1/images/status?ref="+url.QueryEscape("mine:v1"), nil)
	if out["source"] != string(store.ImageImported) {
		t.Errorf("source = %v, want imported", out["source"])
	}
	if out["owner"] != "user-a" {
		t.Errorf("owner = %v, want user-a", out["owner"])
	}
}

func TestImageStatusDefaultsUnsetSourceToImported(t *testing.T) {
	env := startEnv(t, envOpts{})
	// A record from before the source field was populated must not read as an
	// absence of origin: nothing but a platform build ever set it.
	if err := env.Store.PutImage(&store.Image{
		Ref: "legacy:v1", State: store.ImageReady,
	}); err != nil {
		t.Fatal(err)
	}
	_, out := env.do("GET", "/v1/images/status?ref="+url.QueryEscape("legacy:v1"), nil)
	if out["source"] != string(store.ImageImported) {
		t.Errorf("source = %v, want imported", out["source"])
	}
	if out["owner"] != "" {
		t.Errorf("owner = %v, want empty", out["owner"])
	}
}

func TestBuildAttributesImageToCaller(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})
	// The build will not finish here (the local runtime has no BuildKit), but
	// the record is written before the node is asked, which is what ownership
	// attribution rides on.
	resp, out := env.doAs("user-a", "POST", "/v1/images/build", map[string]any{
		"tag": "user-a/app:v1", "dockerfile": "FROM scratch\n",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("build status = %d: %v", resp.StatusCode, out)
	}
	img, err := env.Store.GetImage("user-a/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Fatal("build did not record an image")
	}
	if img.Owner != "user-a" {
		t.Errorf("owner = %q, want user-a", img.Owner)
	}
	if img.Source != store.ImageBuilt {
		t.Errorf("source = %q, want built", img.Source)
	}

	// And it shows up in that caller's "what did I build" listing.
	_, out = env.doAs("user-a", "GET", "/v1/images?source=built", nil)
	if _, ok := refsOf(t, out)["user-a/app:v1"]; !ok {
		t.Error("built image missing from the owner's built listing")
	}
	// But not in another caller's.
	_, out = env.doAs("user-b", "GET", "/v1/images?source=built", nil)
	if _, ok := refsOf(t, out)["user-a/app:v1"]; ok {
		t.Error("built image visible to another caller")
	}
}

func TestOwnerFromHeaderTrimsAndDefaults(t *testing.T) {
	f := OwnerFromHeader("")
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(OwnerHeader, "  user-a  ")
	if got := f(r); got != "user-a" {
		t.Errorf("owner = %q, want user-a", got)
	}

	custom := OwnerFromHeader("X-Tenant")
	r2, _ := http.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Tenant", "t1")
	if got := custom(r2); got != "t1" {
		t.Errorf("custom header owner = %q, want t1", got)
	}
	// The default header is not consulted when a custom one is configured.
	r3, _ := http.NewRequest("GET", "/", nil)
	r3.Header.Set(OwnerHeader, "sneaky")
	if got := custom(r3); got != "" {
		t.Errorf("owner = %q, want empty", got)
	}
}
