package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
)

// namesOf collects name->source from a /v1/templates response.
func namesOf(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	got := map[string]string{}
	list, _ := out["templates"].([]any)
	for _, item := range list {
		tpl, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("template entry is not an object: %v", item)
		}
		name, _ := tpl["name"].(string)
		source, _ := tpl["source"].(string)
		got[name] = source
	}
	return got
}

func TestCreateWithNoPolicyAllowsAnyImage(t *testing.T) {
	// The default has to be today's behaviour: an operator who sets no policy
	// sees no new refusals.
	env := startEnv(t, envOpts{})
	resp, out := env.do("POST", "/v1/sandboxes",
		map[string]any{"imageRef": "ghcr.io/anyone/anything:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
}

func TestCreateRejectsImageOutsideRegistryAllowlist(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedRegistries: []string{"registry.example.com"}},
	})
	resp, out := env.do("POST", "/v1/sandboxes",
		map[string]any{"imageRef": "ghcr.io/outside/app:v1"})
	// A policy refusal is not a 500 and not a malformed request: the caller is
	// asking for something this deployment does not permit.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "IMAGE_NOT_PERMITTED" {
		t.Errorf("code = %v, want IMAGE_NOT_PERMITTED", code)
	}
	// Nothing was registered, so a refused image does not pollute the listing.
	_, out = env.do("GET", "/v1/templates", nil)
	if names := namesOf(t, out); len(names) != 0 {
		t.Errorf("refused image registered: %v", names)
	}
}

func TestCreateAllowsImageInsideRegistryAllowlist(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedRegistries: []string{"index.docker.io"}},
	})
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "python:3.12"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", resp.StatusCode, out)
	}
}

func TestCreateRejectsConvertedImageWhenOnlyBuiltAllowed(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedSources: []store.TemplateSource{store.TemplateBuilt}},
	})
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "python:3.12"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %v", resp.StatusCode, out)
	}
}

func TestCreateAllowsPlatformBuiltTemplateWhenOnlyBuiltAllowed(t *testing.T) {
	env := startEnv(t, envOpts{
		ImagePolicy: image.Policy{AllowedSources: []store.TemplateSource{store.TemplateBuilt}},
	})
	// A platform-built template is on record as built, so the same policy that
	// refuses a conversion lets this through.
	if err := env.Store.PutTemplate(&store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "ours/app:v1",
		Source: store.TemplateBuilt, State: store.TemplateReady,
	}); err != nil {
		t.Fatal(err)
	}
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "ours/app:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", resp.StatusCode, out)
	}
}

func TestTemplateListScopedToCallerIdentity(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})

	// Two callers each run their own image, plus one they share.
	for _, tc := range []struct{ owner, ref string }{
		{"user-a", "a-only:v1"},
		{"user-b", "b-only:v1"},
	} {
		resp, out := env.doAs(tc.owner, "POST", "/v1/sandboxes", map[string]any{"imageRef": tc.ref})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: %d %v", tc.ref, resp.StatusCode, out)
		}
	}

	_, out := env.doAs("user-a", "GET", "/v1/templates", nil)
	names := namesOf(t, out)
	if _, ok := names["a-only:v1"]; !ok {
		t.Error("caller cannot see their own template")
	}
	if _, ok := names["b-only:v1"]; ok {
		t.Error("another caller's template leaked into the listing")
	}

	// An unscoped request is the operator's view and sees both.
	_, out = env.do("GET", "/v1/templates", nil)
	if names := namesOf(t, out); len(names) != 2 {
		t.Errorf("operator view = %v, want both templates", names)
	}
}

func TestTemplateListWithoutIdentityIsUnfiltered(t *testing.T) {
	// No identity configured means every template is unowned and every listing
	// is unfiltered, which is exactly the behaviour before ownership existed.
	env := startEnv(t, envOpts{})
	env.createSandbox(map[string]any{"imageRef": "one:v1"})
	env.createSandbox(map[string]any{"imageRef": "two:v1"})

	_, out := env.doAs("user-a", "GET", "/v1/templates", nil)
	if names := namesOf(t, out); len(names) != 2 {
		t.Errorf("templates = %v, want both despite the header", names)
	}
}

func TestTemplateListFiltersBySource(t *testing.T) {
	env := startEnv(t, envOpts{})
	env.createSandbox(map[string]any{"imageRef": "pulled:v1"})
	if err := env.Store.PutTemplate(&store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "built:v1",
		Source: store.TemplateBuilt, State: store.TemplateReady,
	}); err != nil {
		t.Fatal(err)
	}

	_, out := env.do("GET", "/v1/templates?source=built", nil)
	names := namesOf(t, out)
	if len(names) != 1 || names["built:v1"] != string(store.TemplateBuilt) {
		t.Errorf("built listing = %v", names)
	}

	_, out = env.do("GET", "/v1/templates?source=converted", nil)
	names = namesOf(t, out)
	if len(names) != 1 || names["pulled:v1"] != string(store.TemplateConverted) {
		t.Errorf("converted listing = %v", names)
	}

	resp, _ := env.do("GET", "/v1/templates?source=nonsense", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown source status = %d, want 400", resp.StatusCode)
	}
}

func TestTemplateStatusReportsOriginAndOwner(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})
	resp, out := env.doAs("user-a", "POST", "/v1/sandboxes",
		map[string]any{"imageRef": "mine:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}

	_, out = env.doAs("user-a", "GET",
		"/v1/templates/status?name="+url.QueryEscape("mine:v1"), nil)
	if out["source"] != string(store.TemplateConverted) {
		t.Errorf("source = %v, want converted", out["source"])
	}
	if out["owner"] != "user-a" {
		t.Errorf("owner = %v, want user-a", out["owner"])
	}
}

func TestTemplateStatusDefaultsUnsetSourceToConverted(t *testing.T) {
	env := startEnv(t, envOpts{})
	// A record from before the source field was populated must not read as an
	// absence of origin: nothing but a platform build ever set it.
	if err := env.Store.PutTemplate(&store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "legacy:v1", State: store.TemplateReady,
	}); err != nil {
		t.Fatal(err)
	}
	_, out := env.do("GET", "/v1/templates/status?name="+url.QueryEscape("legacy:v1"), nil)
	if out["source"] != string(store.TemplateConverted) {
		t.Errorf("source = %v, want converted", out["source"])
	}
	if out["owner"] != "" {
		t.Errorf("owner = %v, want empty", out["owner"])
	}
}

func TestBuildAttributesTemplateToCaller(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})
	// The build will not finish here (the local runtime has no BuildKit), but
	// the record is written before the node is asked, which is what ownership
	// attribution rides on.
	resp, out := env.doAs("user-a", "POST", "/v1/templates/build", map[string]any{
		"tag": "user-a/app:v1", "dockerfile": "FROM scratch\n",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("build status = %d: %v", resp.StatusCode, out)
	}
	tpl, err := env.Store.GetTemplateByName("user-a/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if tpl == nil {
		t.Fatal("build did not record a template")
	}
	if tpl.Owner != "user-a" {
		t.Errorf("owner = %q, want user-a", tpl.Owner)
	}
	if tpl.Source != store.TemplateBuilt {
		t.Errorf("source = %q, want built", tpl.Source)
	}

	// And it shows up in that caller's "what did I build" listing.
	_, out = env.doAs("user-a", "GET", "/v1/templates?source=built", nil)
	if _, ok := namesOf(t, out)["user-a/app:v1"]; !ok {
		t.Error("built template missing from the owner's built listing")
	}
	// But not in another caller's.
	_, out = env.doAs("user-b", "GET", "/v1/templates?source=built", nil)
	if _, ok := namesOf(t, out)["user-a/app:v1"]; ok {
		t.Error("built template visible to another caller")
	}
}

func TestDeleteTemplateRemovesTheRecord(t *testing.T) {
	env := startEnv(t, envOpts{})
	if err := env.Store.PutTemplate(&store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "gone:v1",
		Source: store.TemplateBuilt, State: store.TemplateReady,
	}); err != nil {
		t.Fatal(err)
	}

	resp, out := env.do("DELETE", "/v1/templates?name="+url.QueryEscape("gone:v1"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %v", resp.StatusCode, out)
	}
	if out["deleted"] != true {
		t.Errorf("delete body = %v", out)
	}
	resp, _ = env.do("GET", "/v1/templates/status?name="+url.QueryEscape("gone:v1"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status after delete = %d, want 404", resp.StatusCode)
	}

	// A missing name is a 404, and a missing name query is a 400.
	resp, _ = env.do("DELETE", "/v1/templates?name="+url.QueryEscape("never:v1"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing = %d, want 404", resp.StatusCode)
	}
	resp, _ = env.do("DELETE", "/v1/templates", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("delete without id or name = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteTemplateIsScopedToOwner(t *testing.T) {
	env := startEnv(t, envOpts{WithIdentity: true})
	if err := env.Store.PutTemplate(&store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "theirs:v1",
		Source: store.TemplateBuilt, State: store.TemplateReady, Owner: "user-b",
	}); err != nil {
		t.Fatal(err)
	}

	// Another caller may not delete it, and is told it does not exist rather than
	// that it may not touch it.
	resp, _ := env.doAs("user-a", "DELETE", "/v1/templates?name="+url.QueryEscape("theirs:v1"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-owner delete = %d, want 404", resp.StatusCode)
	}
	if tpl, _ := env.Store.GetTemplateByName("theirs:v1"); tpl == nil {
		t.Error("cross-owner delete removed the template")
	}

	// The owner deletes it.
	resp, _ = env.doAs("user-b", "DELETE", "/v1/templates?name="+url.QueryEscape("theirs:v1"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("owner delete = %d, want 200", resp.StatusCode)
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
