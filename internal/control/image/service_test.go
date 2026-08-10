package image

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/garysng/bean/internal/control/store"
)

// fakeCache reports fixed per-ref cache counts.
type fakeCache map[string]int

func (f fakeCache) CachedNodeCount(ref string) int { return f[ref] }

func newSvc(t *testing.T, cache NodeCacheSource) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "img.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, cache), st
}

func TestResolveRegistersOnce(t *testing.T) {
	svc, _ := newSvc(t, nil)
	img, err := svc.Resolve("python:3.12")
	if err != nil {
		t.Fatal(err)
	}
	if img.Name != "python:3.12" {
		t.Errorf("name = %q", img.Name)
	}
	// A new reference starts PENDING: nothing has been converted yet, but
	// the standard pull path can still run it.
	if img.State != store.TemplatePending {
		t.Errorf("state = %s, want PENDING", img.State)
	}
	if img.CreatedAt.IsZero() {
		t.Error("createdAt not set")
	}

	// Resolving again returns the same record, not a duplicate.
	again, err := svc.Resolve("python:3.12")
	if err != nil {
		t.Fatal(err)
	}
	if !again.CreatedAt.Equal(img.CreatedAt) {
		t.Error("second Resolve created a new record")
	}
	all, _ := svc.List()
	if len(all) != 1 {
		t.Errorf("images = %d, want 1", len(all))
	}
}

func TestResolveRejectsBadRefs(t *testing.T) {
	svc, _ := newSvc(t, nil)
	for _, ref := range []string{"", "has space", "-leading-dash", string(make([]byte, 600))} {
		if _, err := svc.Resolve(ref); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ref %q: err = %v, want ErrInvalidRef", ref, err)
		}
	}
}

func TestGetUnknownReturnsNil(t *testing.T) {
	svc, _ := newSvc(t, nil)
	img, err := svc.Get("never:seen")
	if err != nil {
		t.Fatal(err)
	}
	if img != nil {
		t.Errorf("got %+v, want nil", img)
	}
}

func TestConversionStateTransitions(t *testing.T) {
	svc, _ := newSvc(t, nil)
	svc.Resolve("app:1")

	if err := svc.MarkConverting("app:1"); err != nil {
		t.Fatal(err)
	}
	img, _ := svc.Get("app:1")
	if img.State != store.TemplateConverting {
		t.Fatalf("state = %s", img.State)
	}

	cfg := &store.Config{Env: []string{"PATH=/usr/bin"}, Entrypoint: []string{"/bin/sh"}}
	if err := svc.MarkReady("app:1", "app:1-obd", 12345, []string{"sha256:aaa"}, "sha256:oci", cfg); err != nil {
		t.Fatal(err)
	}
	img, _ = svc.Get("app:1")
	if img.State != store.TemplateReady {
		t.Errorf("state = %s, want READY", img.State)
	}
	if img.FS.Digest != "app:1-obd" || img.FS.SizeBytes != 12345 {
		t.Errorf("artifact not recorded: %+v", img)
	}
	if len(img.FS.LayerDigests) != 1 || img.FS.LayerDigests[0] != "sha256:aaa" {
		t.Errorf("layer digests not recorded: %+v", img.FS.LayerDigests)
	}
	// The OCI content digest completes the conversion-cache key.
	if img.OCISource == nil || img.OCISource.Digest != "sha256:oci" {
		t.Errorf("oci source not recorded: %+v", img.OCISource)
	}
	// The recovered image config is recorded on the template.
	if img.FS.Config == nil || len(img.FS.Config.Env) != 1 || img.FS.Config.Env[0] != "PATH=/usr/bin" {
		t.Errorf("config not recorded: %+v", img.FS.Config)
	}

	if err := svc.MarkFailed("app:1", "converter crashed"); err != nil {
		t.Fatal(err)
	}
	img, _ = svc.Get("app:1")
	if img.State != store.TemplateFailed || img.Reason != "converter crashed" {
		t.Errorf("failure not recorded: %+v", img)
	}
}

func TestTransitionUnknownImage(t *testing.T) {
	svc, _ := newSvc(t, nil)
	if err := svc.MarkReady("nope:1", "x", 1, nil, "", nil); err == nil {
		t.Error("expected error for unregistered template")
	}
}

func TestCacheCountIsLive(t *testing.T) {
	cache := fakeCache{"python:3.12": 7}
	svc, _ := newSvc(t, cache)
	svc.Resolve("python:3.12")

	img, _ := svc.Get("python:3.12")
	if img.CachedNodes != 7 {
		t.Errorf("cachedNodes = %d, want 7", img.CachedNodes)
	}
	// The count follows the cache, not the persisted record.
	cache["python:3.12"] = 2
	img, _ = svc.Get("python:3.12")
	if img.CachedNodes != 2 {
		t.Errorf("cachedNodes = %d, want 2 after cache change", img.CachedNodes)
	}
}

func TestPrewarmRegistersAndReportsReadiness(t *testing.T) {
	cache := fakeCache{"a:1": 3, "b:1": 1}
	svc, _ := newSvc(t, cache)

	job, err := svc.Prewarm(PrewarmRequest{
		Refs: []string{"a:1", "b:1"}, Region: "r1", TargetNodes: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Error("no job id")
	}
	if job.Priority != "normal" {
		t.Errorf("priority = %q, want normal default", job.Priority)
	}
	// Both images are registered as a side effect, so status has metadata.
	if imgs, _ := svc.List(); len(imgs) != 2 {
		t.Errorf("images = %d, want 2", len(imgs))
	}
	// b:1 is short of the target, so the job is not done.
	if job.Ready["a:1"] != 3 || job.Ready["b:1"] != 1 {
		t.Errorf("ready = %v", job.Ready)
	}
	if job.Done {
		t.Error("job reported done while b:1 is below target")
	}

	// Once every image reaches the target the job completes.
	cache["b:1"] = 4
	job2, err := svc.JobStatus(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !job2.Done {
		t.Errorf("job not done: %+v", job2)
	}
}

func TestPrewarmValidation(t *testing.T) {
	svc, _ := newSvc(t, nil)
	if _, err := svc.Prewarm(PrewarmRequest{}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("empty refs: err = %v", err)
	}
	if _, err := svc.Prewarm(PrewarmRequest{Refs: []string{"ok:1", "bad ref"}}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("bad ref: err = %v", err)
	}
}

func TestResolveForRecordsOwnerAndSource(t *testing.T) {
	svc, _ := newSvc(t, nil)
	img, err := svc.ResolveFor("python:3.12", "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if img.Owner != "user-a" {
		t.Errorf("owner = %q, want user-a", img.Owner)
	}
	// A caller-supplied ref is a conversion, and saying so is what makes the
	// distinction answerable later.
	if img.Source != store.TemplateConverted {
		t.Errorf("source = %q, want converted", img.Source)
	}
}

func TestResolveForDoesNotReassignOwnership(t *testing.T) {
	svc, _ := newSvc(t, nil)
	if _, err := svc.ResolveFor("shared:v1", "user-a"); err != nil {
		t.Fatal(err)
	}
	// A second caller running the same base image must not take it over.
	img, err := svc.ResolveFor("shared:v1", "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if img.Owner != "user-a" {
		t.Errorf("owner = %q, want the first claimant user-a", img.Owner)
	}
}

func TestResolveWithoutIdentityLeavesImageUnowned(t *testing.T) {
	svc, _ := newSvc(t, nil)
	img, err := svc.Resolve("python:3.12")
	if err != nil {
		t.Fatal(err)
	}
	if img.Owner != "" {
		t.Errorf("owner = %q, want empty", img.Owner)
	}
}

func TestListForScopesToCaller(t *testing.T) {
	svc, _ := newSvc(t, nil)
	svc.ResolveFor("a:1", "user-a")
	svc.ResolveFor("b:1", "user-b")
	svc.Resolve("shared:1")

	mine, err := svc.ListFor("user-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Errorf("images = %d, want 2 (own + unowned)", len(mine))
	}
	// The unscoped list stays the operator's view.
	all, err := svc.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("all images = %d, want 3", len(all))
	}
}

func TestDeleteForScopesToOwner(t *testing.T) {
	svc, _ := newSvc(t, nil)
	svc.ResolveFor("a:1", "user-a")
	svc.ResolveFor("b:1", "user-b")
	svc.Resolve("shared:1")

	// Another identity's image is refused, and refused as not-found so its
	// existence does not leak.
	if err := svc.DeleteFor("b:1", "user-a"); !errors.Is(err, ErrForbidden) {
		t.Errorf("deleting another owner's image = %v, want ErrForbidden", err)
	}
	if img, _ := svc.Get("b:1"); img == nil {
		t.Error("a forbidden delete removed the image anyway")
	}

	// An owner deletes its own, and anyone deletes an unowned one.
	if err := svc.DeleteFor("a:1", "user-a"); err != nil {
		t.Errorf("deleting own image: %v", err)
	}
	if img, _ := svc.Get("a:1"); img != nil {
		t.Error("own image survived delete")
	}
	if err := svc.DeleteFor("shared:1", "user-a"); err != nil {
		t.Errorf("deleting unowned image: %v", err)
	}

	// An unknown ref is reported as not-found.
	if err := svc.DeleteFor("missing:1", "user-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing image = %v, want ErrNotFound", err)
	}

	// The operator (empty owner) may delete anything.
	if err := svc.Delete("b:1"); err != nil {
		t.Errorf("operator delete: %v", err)
	}
	if img, _ := svc.Get("b:1"); img != nil {
		t.Error("operator delete did not remove the image")
	}
}

func TestServicePolicyRefusesDeniedRef(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "img.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := NewWithPolicy(st, nil, Policy{
		AllowedSources: []store.TemplateSource{store.TemplateBuilt},
	})

	if _, err := svc.ResolveFor("python:3.12", "user-a"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	// A refused reference must not be registered: recording it would make the
	// platform's own image list grow with things it declined to run.
	if imgs, _ := svc.List(); len(imgs) != 0 {
		t.Errorf("refused ref was registered: %+v", imgs)
	}
}

func TestServicePolicyAppliesToAlreadyRegisteredRef(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "img.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Registered while the deployment was permissive.
	permissive := New(st, nil)
	if _, err := permissive.Resolve("ghcr.io/x/y:1"); err != nil {
		t.Fatal(err)
	}

	// Tightening the policy has to bite on the next create, not only on refs
	// nobody has run yet.
	strict := NewWithPolicy(st, nil, Policy{AllowedRegistries: []string{"reg.example.com"}})
	if _, err := strict.Resolve("ghcr.io/x/y:1"); !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("err = %v, want ErrPolicyDenied for an already-registered ref", err)
	}
}

func TestJobStatusUnknown(t *testing.T) {
	svc, _ := newSvc(t, nil)
	job, err := svc.JobStatus("pw_missing")
	if err != nil {
		t.Fatal(err)
	}
	if job != nil {
		t.Errorf("got %+v, want nil", job)
	}
}
