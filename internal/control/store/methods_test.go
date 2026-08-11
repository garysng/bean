package store

import (
	"testing"
	"time"
)

func TestListSnapshotsFiltersByStateAndLabel(t *testing.T) {
	st := openTestStore(t)
	seed := []*Snapshot{
		{ID: "snap-a", SandboxID: "sbx", State: SnapshotReady, Labels: map[string]string{"suite": "x"}},
		{ID: "snap-b", SandboxID: "sbx", State: SnapshotReady, Labels: map[string]string{"suite": "y"}},
		{ID: "snap-c", SandboxID: "sbx", State: SnapshotFailed, Labels: map[string]string{"suite": "x"}},
	}
	for _, s := range seed {
		if err := st.PutSnapshot(s); err != nil {
			t.Fatal(err)
		}
	}

	// No filters returns everything.
	all, err := st.ListSnapshots("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered = %d, want 3", len(all))
	}

	// State filter drops the failed one.
	ready, err := st.ListSnapshots("", "", SnapshotReady)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 2 {
		t.Fatalf("ready = %d, want 2", len(ready))
	}

	// Label filter narrows to one; combined with state it is an AND.
	labelled, err := st.ListSnapshots("suite", "x", SnapshotReady)
	if err != nil {
		t.Fatal(err)
	}
	if len(labelled) != 1 || labelled[0].ID != "snap-a" {
		t.Fatalf("labelled ready = %v, want [snap-a]", labelled)
	}
}

func TestDeleteTemplateRemovesTheRecord(t *testing.T) {
	st := openTestStore(t)
	in := tpl("gone:v1", "user-a", TemplateBuilt)
	if err := st.PutTemplate(in); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTemplate(in.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTemplate(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("template still present after delete: %+v", got)
	}
	// Deleting a missing template is not an error, so cleanup stays idempotent.
	if err := st.DeleteTemplate("tpl_absent"); err != nil {
		t.Errorf("delete missing template = %v, want nil", err)
	}
}

// TestTemplateFSArtifactAndOCISourceRoundTrip is the load-bearing shape of the
// rename work: a template embeds an FSArtifact (the overlaybd fs key + chain +
// config) and an optional OCISource (the conversion-cache key). Both must
// survive a store round-trip or a create-by-imageRef reuse cannot find them.
func TestTemplateFSArtifactAndOCISourceRoundTrip(t *testing.T) {
	st := openTestStore(t)
	in := &Template{
		ID: NewID(PrefixTemplate), Name: "python:2.3", State: TemplateReady,
		Source: TemplateConverted, CreatedAt: time.Now(),
		FS: FSArtifact{
			Digest:       "sha256:fsmanifest",
			LayerDigests: []string{"sha256:base", "sha256:top"},
			SizeBytes:    4096,
			Config:       &Config{Env: []string{"A=1"}, Cmd: []string{"python"}, WorkingDir: "/w"},
		},
		OCISource: &OCISource{Ref: "python:2.3", Digest: "sha256:ocicontent"},
	}
	if err := st.PutTemplate(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTemplateByName("python:2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("template not resolvable by name")
	}
	if got.FS.Digest != "sha256:fsmanifest" || len(got.FS.LayerDigests) != 2 || got.FS.SizeBytes != 4096 {
		t.Errorf("FSArtifact did not round-trip: %+v", got.FS)
	}
	if got.FS.Config == nil || got.FS.Config.WorkingDir != "/w" {
		t.Errorf("config did not round-trip: %+v", got.FS.Config)
	}
	if got.OCISource == nil || got.OCISource.Ref != "python:2.3" || got.OCISource.Digest != "sha256:ocicontent" {
		t.Errorf("OCISource did not round-trip: %+v", got.OCISource)
	}
}

func TestBuildCRUD(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()
	b := &ImageBuild{
		ID: NewID(PrefixBuild), State: BuildPending, CreatedAt: now,
		Plan: &BuildPlan{From: "alpine:3.20", Tag: "app:v1", Kind: BuildKindDockerfile},
	}
	if err := st.PutBuild(b); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBuild(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Plan == nil || got.Plan.Tag != "app:v1" {
		t.Fatalf("build did not round-trip: %+v", got)
	}
	// A missing build is (nil, nil), not an error.
	if miss, err := st.GetBuild("bld_absent"); err != nil || miss != nil {
		t.Errorf("GetBuild(absent) = %v, %v; want nil, nil", miss, err)
	}

	// Move it to READY and add a second, so the state filter has something to cut.
	b.State = BuildReady
	if err := st.PutBuild(b); err != nil {
		t.Fatal(err)
	}
	b2 := &ImageBuild{ID: NewID(PrefixBuild), State: BuildFailed, CreatedAt: now.Add(time.Second),
		Plan: &BuildPlan{Tag: "app:v2"}}
	if err := st.PutBuild(b2); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListBuilds("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all builds = %d, want 2", len(all))
	}
	ready, err := st.ListBuilds(BuildReady)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].State != BuildReady {
		t.Fatalf("ready builds = %v, want one READY", ready)
	}
}

func TestPrewarmJobCRUD(t *testing.T) {
	st := openTestStore(t)
	job := &PrewarmJob{
		ID: NewID(PrefixPrewarmJob), Refs: []string{"busybox:1.36", "alpine:3.20"},
		TargetNodes: 2, CreatedAt: time.Now(), Ready: map[string]int{"busybox:1.36": 1},
	}
	if err := st.PutPrewarmJob(job); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPrewarmJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Refs) != 2 || got.Ready["busybox:1.36"] != 1 {
		t.Fatalf("prewarm job did not round-trip: %+v", got)
	}
	if miss, err := st.GetPrewarmJob("pw_absent"); err != nil || miss != nil {
		t.Errorf("GetPrewarmJob(absent) = %v, %v; want nil, nil", miss, err)
	}
}

func TestScanTemplateRejectsCorruptBlob(t *testing.T) {
	st := openTestStore(t)
	// Write a row whose data column is not valid JSON, bypassing PutTemplate so
	// the decode failure surfaces on read rather than being prevented on write.
	if _, err := st.exec(
		`INSERT INTO templates(id, name, data, state, updated_at, owner) VALUES(?,?,?,?,?,?)`,
		"tpl_corrupt", "bad:v1", "{not json", "READY", time.Now().UnixNano(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTemplate("tpl_corrupt"); err == nil {
		t.Error("GetTemplate on a corrupt blob returned no error")
	}
}

func TestNewIDIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID(PrefixTemplate)
		if len(id) <= len(PrefixTemplate)+1 || id[:len(PrefixTemplate)] != PrefixTemplate {
			t.Fatalf("id %q is not prefixed with %q", id, PrefixTemplate)
		}
		if seen[id] {
			t.Fatalf("NewID returned a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestTypeHelpers(t *testing.T) {
	if got := AllSandboxStates(); len(got) != 12 {
		t.Errorf("AllSandboxStates returned %d states, want 12", len(got))
	}
	for _, s := range []SandboxState{SandboxStopped, SandboxFailed, SandboxLost} {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = false, want true", s)
		}
	}
	for _, s := range []SandboxState{SandboxRunning, SandboxPaused, SandboxPending} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%s) = true, want false", s)
		}
	}
	for _, s := range []BuildState{BuildReady, BuildFailed, BuildCancelled} {
		if !IsBuildTerminal(s) {
			t.Errorf("IsBuildTerminal(%s) = false, want true", s)
		}
	}
	if IsBuildTerminal(BuildPending) {
		t.Error("IsBuildTerminal(PENDING) = true, want false")
	}
}
