package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The limits are written into a tree at a path this package is given, so the
// whole mechanism is exercised against a directory built by the test rather than
// against /sys/fs/cgroup. That is what makes both interface versions testable at
// all: the target host is v1 and no developer machine has both, so a test that
// needed a real tree could only ever cover one of them, and the version-specific
// filenames are exactly where a mistake is silent -- a write to a file that does
// not exist is the only symptom, and nothing reads it back.
//
// What this cannot cover is whether the kernel honours what was written. The
// filename, the value and the teardown are asserted here; enforcement is
// unverified without a real KVM host.

// fakeV1Tree builds a v1 layout: one directory per controller under the root.
func fakeV1Tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range cgroupControllers {
		if err := os.MkdirAll(filepath.Join(root, c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeV2Tree builds a v2 layout: one unified tree, with the controllers
// advertised and delegated the way a v2 parent has to advertise them.
func fakeV2Tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	avail := strings.Join(cgroupControllers, " ") + " cpuset io"
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte(avail), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCgroupV1WritesTheV1FilenamesAndValues is the test that goes red if the
// memory ceiling stops being applied, and the one that goes red if v2's spelling
// is used on a v1 host.
//
// The target host measured for this work is v1 with controllers mounted
// separately: /sys/fs/cgroup is tmpfs, there is no cgroup.controllers, and
// memory.max does not exist anywhere. A VMM on such a host with only memory.max
// written is completely unbounded while every log line says limits are on.
func TestCgroupV1WritesTheV1FilenamesAndValues(t *testing.T) {
	root := fakeV1Tree(t)
	h := newCgroupHost(root, cgroupV1)
	if !h.Enabled() {
		t.Fatal("no controller usable in a tree that has all of them")
	}

	spec := &Spec{SandboxID: "sb1", CPU: 2, MemoryMiB: 1024}
	if _, err := h.createCgroup(spec.SandboxID, limitsFor(spec)); err != nil {
		t.Fatalf("createCgroup: %v", err)
	}

	// memory: 1024 MiB of guest RAM plus the VMM's own headroom.
	wantMem := (1024 + vmmMemoryHeadroomMiB) << 20
	for _, tc := range []struct{ file, want string }{
		{filepath.Join("memory", "bean-sb1", "memory.limit_in_bytes"), "1342177280"},
		{filepath.Join("cpu", "bean-sb1", "cpu.cfs_period_us"), "100000"},
		// 2 cores of a 100ms period.
		{filepath.Join("cpu", "bean-sb1", "cpu.cfs_quota_us"), "200000"},
		{filepath.Join("pids", "bean-sb1", "pids.max"), "512"},
	} {
		b, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Errorf("%s was not written: %v; on a v1 host this limit is not in "+
				"force and nothing reports that", tc.file, err)
			continue
		}
		if got := string(b); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.file, got, tc.want)
		}
	}
	if wantMem != 1342177280 {
		t.Fatalf("headroom arithmetic changed: %d", wantMem)
	}

	// And specifically not v2's names, which would be created as ordinary files by
	// a WriteFile into a directory that exists.
	for _, absent := range []string{"memory.max", "cpu.max"} {
		if _, err := os.Stat(filepath.Join(root, "memory", "bean-sb1", absent)); err == nil {
			t.Errorf("%s exists in a v1 tree: the v2 spelling was used", absent)
		}
	}
}

// TestCgroupV2WritesTheUnifiedFilenames is the mirror: one tree, v2 names.
func TestCgroupV2WritesTheUnifiedFilenames(t *testing.T) {
	root := fakeV2Tree(t)
	h := newCgroupHost(root, cgroupV2)
	if !h.Enabled() {
		t.Fatal("no controller usable in a v2 tree advertising all of them")
	}

	spec := &Spec{SandboxID: "sb2", CPU: 0.5, MemoryMiB: 512}
	if _, err := h.createCgroup(spec.SandboxID, limitsFor(spec)); err != nil {
		t.Fatalf("createCgroup: %v", err)
	}

	dir := filepath.Join(root, "bean-sb2")
	for _, tc := range []struct{ file, want string }{
		{"memory.max", "805306368"},
		{"memory.swap.max", "0"},
		{"cpu.max", "50000 100000"},
		{"pids.max", "512"},
	} {
		b, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Errorf("%s was not written: %v", tc.file, err)
			continue
		}
		if got := string(b); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.file, got, tc.want)
		}
	}

	// Every controller must have been delegated, or a child group has none of the
	// files above and the limits are silently absent.
	//
	// Asserted through the host's own controller set rather than by reading
	// cgroup.subtree_control back: on real cgroupfs a "+name" write accumulates
	// into that file, while on the ordinary filesystem this fake tree is built on
	// each write replaces the last, so the file would only ever show the final
	// controller. The set below is what newCgroupHost concluded, which is the thing
	// that decides whether a limit gets written at all.
	for _, c := range cgroupControllers {
		if !h.has(c) {
			t.Errorf("controller %q was not delegated, so its files do not exist in "+
				"the child group and the limit is silently not applied", c)
		}
	}
	// And the delegation was actually attempted through the documented interface.
	b, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "+") {
		t.Errorf("cgroup.subtree_control = %q, want a +name write: writing the bare "+
			"set would disable controllers this node did not ask about, on a tree it "+
			"shares with systemd", b)
	}
}

// TestV2ControllerNotAdvertisedIsNotUsed covers the v2 host that has the unified
// tree but not the controller: the group must not be created with a limit that
// cannot be expressed. A cpuset-only tree is the realistic version of this.
func TestV2ControllerNotAdvertisedIsNotUsed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpuset io"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newCgroupHost(root, cgroupV2)
	if h.Enabled() {
		t.Errorf("controllers %v were taken as usable from a tree advertising only "+
			"cpuset and io", h.controllers)
	}
	if s := h.Summary(); !strings.Contains(s, "enforcing nothing") {
		t.Errorf("summary does not say nothing is enforced: %q", s)
	}
}

// TestCgroupRemoveLeavesNothingBehind is the leak test. A cgroup directory that
// outlives its sandbox is the same class of bug as the loop-device leak in
// GitHub #16: invisible to everything, and permanent until the host reboots.
func TestCgroupRemoveLeavesNothingBehind(t *testing.T) {
	for name, tc := range map[string]struct {
		root    func(*testing.T) string
		version cgroupVersion
	}{
		"v1": {fakeV1Tree, cgroupV1},
		"v2": {fakeV2Tree, cgroupV2},
	} {
		t.Run(name, func(t *testing.T) {
			root := tc.root(t)
			h := newCgroupHost(root, tc.version)
			g, err := h.createCgroup("sb-leak", limitsFor(&Spec{
				SandboxID: "sb-leak", CPU: 1, MemoryMiB: 256,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if len(g.dirs) == 0 {
				t.Fatal("no directory was created, so this proves nothing")
			}
			created := append([]string(nil), g.dirs...)

			if err := g.Remove(); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			for _, d := range created {
				if _, err := os.Stat(d); err == nil {
					t.Errorf("%s survived teardown: a leaked cgroup is invisible to "+
						"every other subsystem and stays for the life of the host", d)
				}
			}
			// Removing twice must be safe: the failed-create path and Destroy can
			// both reach it, and a second error would mask the first.
			if err := g.Remove(); err != nil {
				t.Errorf("second Remove: %v", err)
			}
		})
	}
}

// TestCreateCgroupCleansUpAfterAFailedWrite covers the failed-create half of the
// same leak. A group half-built and then abandoned is worse than one that leaks
// on destroy: no caller holds a reference to it, so nothing can ever remove it.
func TestCreateCgroupCleansUpAfterAFailedWrite(t *testing.T) {
	root := fakeV1Tree(t)
	// The pids tree is replaced by a regular file, so the mkdir under it fails with
	// ENOTDIR and the group is left half-built after memory and cpu succeeded.
	//
	// A mode-based trigger was tried first and does not work: noded runs as root,
	// the cross-compiled test binary is run as root under Docker, and root ignores
	// the write bit -- so a 0500 directory is still writable and the create
	// succeeded. That is exactly the false pass this whole verification pass exists
	// to catch, and it only showed up when the Linux binary was actually run.
	pidsTree := filepath.Join(root, cgroupPids)
	if err := os.RemoveAll(pidsTree); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidsTree, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &cgroupHost{root: root, version: cgroupV1, controllers: cgroupControllers}
	if _, err := h.createCgroup("sb-partial", limitsFor(&Spec{
		SandboxID: "sb-partial", CPU: 1, MemoryMiB: 256,
	})); err == nil {
		t.Fatal("createCgroup succeeded with an unwritable controller tree")
	}
	for _, c := range []string{cgroupMemory, cgroupCPU} {
		if _, err := os.Stat(filepath.Join(root, c, "bean-sb-partial")); err == nil {
			t.Errorf("%s/bean-sb-partial survived a failed create: nothing holds a "+
				"reference to it, so nothing will ever remove it", c)
		}
	}
}

// TestSweepOrphansRemovesOnlyBeansOwnGroups pins the boundary the startup sweep
// depends on. The tree is shared with systemd, Docker and anything else on the
// host, and removing one of theirs is not recoverable by restarting anything.
func TestSweepOrphansRemovesOnlyBeansOwnGroups(t *testing.T) {
	root := fakeV1Tree(t)
	h := newCgroupHost(root, cgroupV1)

	strangers := []string{"docker", "system.slice", "kubepods"}
	for _, c := range cgroupControllers {
		for _, s := range strangers {
			if err := os.MkdirAll(filepath.Join(root, c, s), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := h.createCgroup("sb-old", limitsFor(&Spec{SandboxID: "sb-old", MemoryMiB: 128})); err != nil {
		t.Fatal(err)
	}

	removed, inUse := h.SweepOrphans()
	if removed != len(cgroupControllers) {
		t.Errorf("swept %d directories, want %d (one per controller)", removed, len(cgroupControllers))
	}
	if inUse != 0 {
		t.Errorf("%d directories reported in use; nothing here holds a process", inUse)
	}
	for _, c := range cgroupControllers {
		if _, err := os.Stat(filepath.Join(root, c, "bean-sb-old")); err == nil {
			t.Errorf("%s/bean-sb-old survived the sweep", c)
		}
		for _, s := range strangers {
			if _, err := os.Stat(filepath.Join(root, c, s)); err != nil {
				t.Errorf("the sweep removed %s/%s, which is not bean's: %v", c, s, err)
			}
		}
	}
}

// TestCgroupNameRefusesPathsGuards the one place a control-plane string becomes a
// path that is later removed.
func TestCgroupNameRefusesPaths(t *testing.T) {
	for _, id := range []string{"", "..", ".", "a/b", "../../etc", "x\x00y"} {
		if _, err := cgroupNameFor(id); err == nil {
			t.Errorf("cgroupNameFor(%q) was accepted: it becomes a directory that "+
				"teardown removes, so it must be one path element", id)
		}
	}
	got, err := cgroupNameFor("sb-1")
	if err != nil || got != "bean-sb-1" {
		t.Errorf("cgroupNameFor(sb-1) = %q, %v; want bean-sb-1", got, err)
	}
	// The inverse must agree with it, for the same reason
	// image.SandboxIDFromDMName exists: the sweep decides what to remove from this
	// answer, and a disagreement either leaks groups or removes a stranger's.
	if id, ok := sandboxIDFromCgroupName(got); !ok || id != "sb-1" {
		t.Errorf("sandboxIDFromCgroupName(%q) = %q, %v; want sb-1, true", got, id, ok)
	}
	for _, notOurs := range []string{"docker", "system.slice", "bean-", "beanx"} {
		if _, ok := sandboxIDFromCgroupName(notOurs); ok {
			t.Errorf("sandboxIDFromCgroupName(%q) claimed it as bean's", notOurs)
		}
	}
}

// TestNilCgroupHostIsTheUntouchedPath is the configuration every existing
// deployment runs: nothing configured, nothing created, nothing limited, and no
// error anywhere. A nil host must behave as if this file did not exist.
func TestNilCgroupHostIsTheUntouchedPath(t *testing.T) {
	var h *cgroupHost
	if h.Enabled() {
		t.Error("a nil host reports limits enabled")
	}
	g, err := h.createCgroup("sb", limitsFor(&Spec{SandboxID: "sb", CPU: 4, MemoryMiB: 2048}))
	if err != nil {
		t.Fatalf("createCgroup on a nil host: %v", err)
	}
	if g != nil {
		t.Fatalf("a nil host produced a group: %+v", g)
	}
	// Every method on the resulting nil group has to be safe, because the launch
	// path calls them unconditionally.
	if err := g.Add(1); err != nil {
		t.Errorf("Add on a nil group: %v", err)
	}
	if err := g.Remove(); err != nil {
		t.Errorf("Remove on a nil group: %v", err)
	}
	if removed, inUse := h.SweepOrphans(); removed != 0 || inUse != 0 {
		t.Errorf("a nil host swept %d/%d", removed, inUse)
	}
	// And an empty-but-present host, which is the "cgroups requested, no usable
	// controller" case: a node in it starts and runs unlimited.
	empty := &cgroupHost{root: t.TempDir(), version: cgroupV1}
	if empty.Enabled() {
		t.Error("a host with no controllers reports limits enabled")
	}
	if g, err := empty.createCgroup("sb", cgroupLimits{}); err != nil || g != nil {
		t.Errorf("createCgroup with no controllers = %+v, %v; want nil, nil", g, err)
	}
}

// TestSummaryStatesWhatIsNotEnforced guards against the failure mode the A3
// documentation error had: a reader concluding a limit is in force when it is
// not. An operator raises --overcommit-memory on the strength of this line.
func TestSummaryStatesWhatIsNotEnforced(t *testing.T) {
	var nilHost *cgroupHost
	if s := nilHost.Summary(); !strings.Contains(s, "unlimited") {
		t.Errorf("nil host summary does not say the VMM is unlimited: %q", s)
	}

	root := fakeV1Tree(t)
	if err := os.RemoveAll(filepath.Join(root, cgroupMemory)); err != nil {
		t.Fatal(err)
	}
	h := newCgroupHost(root, cgroupV1)
	s := h.Summary()
	if !strings.Contains(s, "unavailable") || !strings.Contains(s, cgroupMemory) {
		t.Errorf("summary does not name the missing memory controller: %q", s)
	}
	// v1's inability to cap swap is a real gap and has to be stated rather than
	// left as an absence.
	if !strings.Contains(s, "swap") {
		t.Errorf("v1 summary does not mention that swap is uncapped: %q", s)
	}
}

// TestLimitsForUsesTheSandboxSpec pins that the ceiling and the guest are two
// views of one number. A sandbox with no stated size gets no ceiling: inventing
// one would kill it on a figure nothing in the request explains.
func TestLimitsForUsesTheSandboxSpec(t *testing.T) {
	l := limitsFor(&Spec{SandboxID: "sb", CPU: 2.5, MemoryMiB: 2048})
	if want := int64(2048+vmmMemoryHeadroomMiB) << 20; l.MemoryBytes != want {
		t.Errorf("MemoryBytes = %d, want %d", l.MemoryBytes, want)
	}
	if l.CPUCores != 2.5 {
		t.Errorf("CPUCores = %v, want 2.5", l.CPUCores)
	}
	if l.PidsMax != cgroupPidsMax {
		t.Errorf("PidsMax = %d, want %d", l.PidsMax, cgroupPidsMax)
	}

	unsized := limitsFor(&Spec{SandboxID: "sb"})
	if unsized.MemoryBytes != 0 || unsized.CPUCores != 0 {
		t.Errorf("an unsized spec got a ceiling: %+v", unsized)
	}
	// pids is the exception, and deliberately: it needs nothing from the spec and
	// bounds a VMM that is forking without bound whatever the sandbox asked for.
	if unsized.PidsMax != cgroupPidsMax {
		t.Errorf("an unsized spec got no pid cap: %+v", unsized)
	}

	// A guest with no memory limit must produce no memory write at all, rather
	// than a limit of the headroom alone -- which would be a ceiling below the
	// 512 MiB configureAndBoot defaults the guest to, and would kill it.
	h := &cgroupHost{root: t.TempDir(), version: cgroupV2, controllers: cgroupControllers}
	if w := h.writesFor(cgroupMemory, unsized); len(w) != 0 {
		t.Errorf("writesFor produced %+v for an unsized spec", w)
	}
}

// TestCPUQuotaIsFlooredAtTheKernelMinimum covers the small-fraction case: the
// kernel rejects a quota below 1ms, and a create must not fail over a sandbox
// asking for a thousandth of a core.
func TestCPUQuotaIsFlooredAtTheKernelMinimum(t *testing.T) {
	h := &cgroupHost{root: t.TempDir(), version: cgroupV2, controllers: cgroupControllers}
	w := h.writesFor(cgroupCPU, cgroupLimits{CPUCores: 0.000001})
	if len(w) != 1 || w[0].value != "1000 100000" {
		t.Errorf("writesFor cpu = %+v, want a quota floored at 1000us", w)
	}
}
