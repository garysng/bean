package reclaim

import (
	"errors"
	"strings"
	"testing"

	"github.com/garysng/bean/internal/obs"
)

const (
	testBase   = "/var/lib/bean/sandboxes"
	testImages = "/var/lib/bean/images"
)

// fakeHost stands in for dmsetup and losetup. Removals are recorded rather than
// performed, which is the point: the decisions are what need testing, and doing
// this for real would need root and would put the machine running the test at
// risk of the very bug being tested for.
type fakeHost struct {
	mappings []string
	loops    []LoopDevice
	dirs     []string

	// busy names a mapping the kernel refuses to remove, standing in for a device
	// still held open by a firecracker process that outlived its noded.
	busy map[string]bool
	// stuckLoops names loop devices the kernel refuses to detach, which is what it
	// does whenever something still has the device open.
	stuckLoops map[string]bool

	listMappingsErr error
	listLoopsErr    error
	listDirsErr     error

	removedDM    []string
	detachedLoop []string
	removedDirs  []string
}

func (f *fakeHost) ListDMNames() ([]string, error) {
	return f.mappings, f.listMappingsErr
}

func (f *fakeHost) RemoveDM(name string) error {
	if f.busy[name] {
		return errors.New("device or resource busy")
	}
	f.removedDM = append(f.removedDM, name)
	return nil
}

func (f *fakeHost) ListLoopDevices() ([]LoopDevice, error) {
	return f.loops, f.listLoopsErr
}

func (f *fakeHost) DetachLoop(dev string) error {
	if f.stuckLoops[dev] {
		return errors.New("device or resource busy")
	}
	f.detachedLoop = append(f.detachedLoop, dev)
	return nil
}

func (f *fakeHost) ListSandboxDirs() ([]string, error) {
	return f.dirs, f.listDirsErr
}

func (f *fakeHost) RemoveSandboxDir(name string) error {
	f.removedDirs = append(f.removedDirs, name)
	return nil
}

func newReconciler(h Host) *Reconciler {
	return &Reconciler{BaseDir: testBase, ImageDir: testImages, Host: h,
		Metrics: obs.NewRegistry()}
}

func cowOf(id string) string { return testBase + "/" + id + "/cow.img" }

func expect(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// TestReclaimsFullOrphanStack is the case from GitHub #17: a killed noded left a
// mapping, the loop device it held and the directory behind that, and the control
// plane no longer expects the sandbox. All three must go, in that order.
func TestReclaimsFullOrphanStack(t *testing.T) {
	h := &fakeHost{
		mappings: []string{"bean-sbx_dead"},
		loops:    []LoopDevice{{Name: "/dev/loop15", BackingFile: cowOf("sbx_dead"), Deleted: true}},
		dirs:     []string{"sbx_dead"},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}

	if !contains(h.removedDM, "bean-sbx_dead") {
		t.Errorf("mapping not removed: %v", h.removedDM)
	}
	if !contains(h.detachedLoop, "/dev/loop15") {
		t.Errorf("loop device not detached: %v", h.detachedLoop)
	}
	if !contains(h.removedDirs, "sbx_dead") {
		t.Errorf("sandbox directory not removed: %v", h.removedDirs)
	}
	for _, kind := range []string{kindMapping, kindLoop, kindDir} {
		if rep.Reclaimed[kind] != 1 || rep.Found[kind] != 1 {
			t.Errorf("%s: found=%d reclaimed=%d, want 1/1",
				kind, rep.Found[kind], rep.Reclaimed[kind])
		}
	}
}

// TestLeavesExpectedSandboxAlone is the property that makes this safe to run at
// all. A sandbox started before this process began is invisible to it, so the
// only thing standing between it and a destroyed filesystem is the expected set.
func TestLeavesExpectedSandboxAlone(t *testing.T) {
	h := &fakeHost{
		mappings: []string{"bean-sbx_live"},
		loops:    []LoopDevice{{Name: "/dev/loop3", BackingFile: cowOf("sbx_live")}},
		dirs:     []string{"sbx_live"},
	}
	rep, err := newReconciler(h).Run(expect("sbx_live"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.removedDM) != 0 || len(h.detachedLoop) != 0 || len(h.removedDirs) != 0 {
		t.Fatalf("touched a running sandbox: dm=%v loops=%v dirs=%v",
			h.removedDM, h.detachedLoop, h.removedDirs)
	}
	for _, kind := range []string{kindMapping, kindLoop, kindDir} {
		if rep.Kept[kind] != 1 {
			t.Errorf("%s kept = %d, want 1", kind, rep.Kept[kind])
		}
		if rep.Found[kind] != 0 {
			t.Errorf("%s found = %d, want 0", kind, rep.Found[kind])
		}
	}
}

// TestIgnoresForeignResources covers the shared-host case. These names and paths
// are taken from what actually runs alongside bean: Docker's thin pools, nexus
// pods and snapd's loop-mounted images.
func TestIgnoresForeignResources(t *testing.T) {
	h := &fakeHost{
		mappings: []string{
			"docker-253:1-pool",
			"nexus-pod-7f3a",
			"vg0-lv_root",
			// Close enough to be dangerous: contains bean's prefix but does not
			// start with it.
			"nexus-bean-sbx_x",
		},
		loops: []LoopDevice{
			{Name: "/dev/loop0", BackingFile: "/var/lib/snapd/snaps/core22.snap"},
			{Name: "/dev/loop1", BackingFile: "/var/lib/docker/devicemapper/data"},
			// A deleted file outside bean's directories is still not bean's to
			// reclaim, however obviously leaked it looks.
			{Name: "/dev/loop2", BackingFile: "/tmp/someone-else.img", Deleted: true},
			// Path traversal that shares BaseDir's textual prefix.
			{Name: "/dev/loop4", BackingFile: testBase + "/../../other/cow.img"},
		},
		dirs: nil,
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.removedDM) != 0 || len(h.detachedLoop) != 0 {
		t.Fatalf("touched another workload's resources: dm=%v loops=%v",
			h.removedDM, h.detachedLoop)
	}
	// Not merely untouched: not counted either. A foreign resource appearing in
	// the report is an invitation for somebody to act on it.
	if rep.Found[kindMapping] != 0 || rep.Kept[kindMapping] != 0 ||
		rep.Found[kindLoop] != 0 || rep.Kept[kindLoop] != 0 {
		t.Errorf("foreign resources appear in the report: %+v", rep)
	}
	if len(rep.Suspect) != 0 {
		t.Errorf("foreign resources reported as suspect: %v", rep.Suspect)
	}
}

// TestLeavesBaseImageLoopAttached checks the interaction with GitHub #16. A base
// image's loop device is read-only and shared by every sandbox on the node, and
// this process cannot count its holders, so detaching one would break every guest
// reading through it.
func TestLeavesBaseImageLoopAttached(t *testing.T) {
	h := &fakeHost{
		loops: []LoopDevice{
			{Name: "/dev/loop7", BackingFile: testImages + "/python_3.12.ext4"},
		},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.detachedLoop) != 0 {
		t.Fatalf("detached a shared base image loop: %v", h.detachedLoop)
	}
	if rep.Kept[kindLoop] != 1 {
		t.Errorf("base loop kept = %d, want 1", rep.Kept[kindLoop])
	}
}

// TestKeepsLoopWhoseMappingSurvived is the ordering rule stated as a decision. A
// mapping that could not be removed is still reading through its loop device, so
// detaching it would corrupt whatever the mapping is serving.
func TestKeepsLoopWhoseMappingSurvived(t *testing.T) {
	h := &fakeHost{
		mappings: []string{"bean-sbx_busy"},
		loops:    []LoopDevice{{Name: "/dev/loop9", BackingFile: cowOf("sbx_busy")}},
		dirs:     []string{"sbx_busy"},
		busy:     map[string]bool{"bean-sbx_busy": true},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.detachedLoop) != 0 {
		t.Errorf("detached a loop device still held by a mapping: %v", h.detachedLoop)
	}
	if len(h.removedDirs) != 0 {
		t.Errorf("removed a directory whose mapping is still standing: %v", h.removedDirs)
	}
	if rep.Failed[kindMapping] != 1 {
		t.Errorf("failed mappings = %d, want 1", rep.Failed[kindMapping])
	}
	// The whole situation has to be visible: three resources are still held and
	// nothing will retry until the next restart.
	if len(rep.Suspect) < 3 {
		t.Errorf("suspect entries = %d, want one per resource left behind: %v",
			len(rep.Suspect), rep.Suspect)
	}
}

// TestUnreadableMappingListStopsEverything: without the mapping list, no loop
// device or directory can be shown to be unreferenced, so the pass must decline
// to remove anything rather than fall back on the deleted-file signal alone.
func TestUnreadableMappingListStopsEverything(t *testing.T) {
	h := &fakeHost{
		listMappingsErr: errors.New("dmsetup: command not found"),
		loops:           []LoopDevice{{Name: "/dev/loop15", BackingFile: cowOf("sbx_a"), Deleted: true}},
		dirs:            []string{"sbx_a"},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.detachedLoop) != 0 || len(h.removedDirs) != 0 {
		t.Fatalf("acted without knowing which mappings exist: loops=%v dirs=%v",
			h.detachedLoop, h.removedDirs)
	}
	if len(rep.Suspect) == 0 {
		t.Error("an unreadable mapping list was not reported")
	}
}

// TestReportsDeletedFileUnderLiveSandbox covers the case where reporting is the
// only correct action: the file is gone so the sandbox can never be checkpointed,
// but the device still serves a guest that is supposed to be running.
func TestReportsDeletedFileUnderLiveSandbox(t *testing.T) {
	h := &fakeHost{
		mappings: []string{"bean-sbx_live"},
		loops:    []LoopDevice{{Name: "/dev/loop5", BackingFile: cowOf("sbx_live"), Deleted: true}},
		dirs:     []string{"sbx_live"},
	}
	rep, err := newReconciler(h).Run(expect("sbx_live"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.detachedLoop) != 0 {
		t.Fatalf("detached the device of a running sandbox: %v", h.detachedLoop)
	}
	found := false
	for _, s := range rep.Suspect {
		if strings.Contains(s, "/dev/loop5") && strings.Contains(s, "deleted") {
			found = true
		}
	}
	if !found {
		t.Errorf("deleted backing file under a live sandbox was not reported: %v", rep.Suspect)
	}
}

// TestReclaimsDirectoryWithNoHostReferences covers a create that died before it
// attached anything: the directory and its sparse store exist, no mapping and no
// loop device do. Nothing can be holding the files, so the disk is reclaimable,
// and leaving it would accumulate a directory per failed create.
func TestReclaimsDirectoryWithNoHostReferences(t *testing.T) {
	h := &fakeHost{dirs: []string{"sbx_halfborn"}}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(h.removedDirs, "sbx_halfborn") {
		t.Errorf("directory with no host references not removed: %v", h.removedDirs)
	}
	if rep.Reclaimed[kindDir] != 1 {
		t.Errorf("dirs reclaimed = %d, want 1", rep.Reclaimed[kindDir])
	}
}

// TestKeepsDirectoryWhoseLoopSurvived: a store still attached to a loop device is
// not reclaimable, and unlike a mapping removal the kernel will not refuse the
// unlink to save us. A detach that fails means something has the device open, so
// the files behind it have to stay.
func TestKeepsDirectoryWhoseLoopSurvived(t *testing.T) {
	h := &fakeHost{
		loops:      []LoopDevice{{Name: "/dev/loop9", BackingFile: cowOf("sbx_held")}},
		dirs:       []string{"sbx_held"},
		stuckLoops: map[string]bool{"/dev/loop9": true},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.removedDirs) != 0 {
		t.Errorf("removed a directory still held by a loop device: %v", h.removedDirs)
	}
	if rep.Failed[kindLoop] != 1 {
		t.Errorf("failed loop detaches = %d, want 1", rep.Failed[kindLoop])
	}
	if len(rep.Suspect) < 2 {
		t.Errorf("suspect entries = %d, want one for the device and one for the "+
			"directory: %v", len(rep.Suspect), rep.Suspect)
	}
}

// TestReclaimsLoopWithNoMappingOverIt is the third case listed in GitHub #17: a
// device backed by a sandbox's store with no mapping reading through it is
// unreferenced, whether or not the file itself was unlinked.
func TestReclaimsLoopWithNoMappingOverIt(t *testing.T) {
	h := &fakeHost{
		loops: []LoopDevice{{Name: "/dev/loop9", BackingFile: cowOf("sbx_stale")}},
		dirs:  []string{"sbx_stale"},
	}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if !contains(h.detachedLoop, "/dev/loop9") {
		t.Errorf("unreferenced loop device not detached: %v", h.detachedLoop)
	}
	// Detaching it is what makes the directory reclaimable in the same pass.
	if !contains(h.removedDirs, "sbx_stale") {
		t.Errorf("directory not removed after its device was detached: %v", h.removedDirs)
	}
	if rep.Reclaimed[kindLoop] != 1 {
		t.Errorf("loops reclaimed = %d, want 1", rep.Reclaimed[kindLoop])
	}
}

// TestUnreadableLoopListStopsDirectoryRemoval: not knowing which loop devices
// exist is not the same as knowing there are none, and only the first permits a
// delete.
func TestUnreadableLoopListStopsDirectoryRemoval(t *testing.T) {
	h := &fakeHost{
		listLoopsErr: errors.New("losetup: permission denied"),
		dirs:         []string{"sbx_a"},
	}
	if _, err := newReconciler(h).Run(expect()); err != nil {
		t.Fatal(err)
	}
	if len(h.removedDirs) != 0 {
		t.Errorf("removed a directory without knowing what holds it: %v", h.removedDirs)
	}
}

// TestSkipsRuntimeStateDirs guards the snapshot cache and the image work
// directory, which live under BaseDir beside the sandboxes. Deleting the snapshot
// cache is survivable but expensive: every restore pays the full unpack again.
func TestSkipsRuntimeStateDirs(t *testing.T) {
	h := &fakeHost{dirs: []string{".snapshots", ".work"}}
	rep, err := newReconciler(h).Run(expect())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.removedDirs) != 0 {
		t.Fatalf("removed runtime state: %v", h.removedDirs)
	}
	if rep.Found[kindDir] != 0 {
		t.Errorf("runtime state counted as orphaned: %d", rep.Found[kindDir])
	}
}

func TestRunWithoutHostFails(t *testing.T) {
	r := &Reconciler{BaseDir: testBase}
	if _, err := r.Run(expect()); err == nil {
		t.Error("expected an error without a host")
	}
}

func TestSandboxIDForPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
		ok   bool
	}{
		{cowOf("sbx_a"), "sbx_a", true},
		{testBase + "/sbx_b/stage/mem", "sbx_b", true},
		// Directly in BaseDir: owned by no sandbox.
		{testBase + "/stray.img", "", false},
		{testBase, "", false},
		// Escapes BaseDir while sharing its prefix.
		{testBase + "/../evil/cow.img", "", false},
		{"/var/lib/bean/sandboxes-other/sbx_c/cow.img", "", false},
		// Escapes from inside a plausible sandbox directory. losetup reports the
		// path it was attached with, which need not be clean, so the check has to
		// resolve the traversal rather than trust the leading component.
		{testBase + "/sbx_a/../../evil/cow.img", "", false},
		// A redundant separator must still resolve to the sandbox, or its
		// resources would silently never be reclaimed.
		{testBase + "//sbx_a/cow.img", "sbx_a", true},
		{"/tmp/cow.img", "", false},
		// Runtime state, not a sandbox.
		{testBase + "/.snapshots/snap_1/mem", "", false},
		{"", "", false},
	} {
		got, ok := sandboxIDForPath(testBase, tc.path)
		if got != tc.want || ok != tc.ok {
			t.Errorf("sandboxIDForPath(%q) = %q,%v want %q,%v",
				tc.path, got, ok, tc.want, tc.ok)
		}
	}
}
