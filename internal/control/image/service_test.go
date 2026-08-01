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
	if img.Ref != "python:3.12" {
		t.Errorf("ref = %q", img.Ref)
	}
	// A new reference starts PENDING: nothing has been converted yet, but
	// the standard pull path can still run it.
	if img.State != store.ImagePending {
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
	if img.State != store.ImageConverting {
		t.Fatalf("state = %s", img.State)
	}

	if err := svc.MarkReady("app:1", "app:1-obd", 12345); err != nil {
		t.Fatal(err)
	}
	img, _ = svc.Get("app:1")
	if img.State != store.ImageReady {
		t.Errorf("state = %s, want READY", img.State)
	}
	if img.OverlaybdRef != "app:1-obd" || img.SizeBytes != 12345 {
		t.Errorf("artifact not recorded: %+v", img)
	}

	if err := svc.MarkFailed("app:1", "converter crashed"); err != nil {
		t.Fatal(err)
	}
	img, _ = svc.Get("app:1")
	if img.State != store.ImageFailed || img.Reason != "converter crashed" {
		t.Errorf("failure not recorded: %+v", img)
	}
}

func TestTransitionUnknownImage(t *testing.T) {
	svc, _ := newSvc(t, nil)
	if err := svc.MarkReady("nope:1", "x", 1); err == nil {
		t.Error("expected error for unregistered image")
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
