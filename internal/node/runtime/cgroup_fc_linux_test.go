//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These cover the launch path rather than the cgroup files: that the VMM's pid
// reaches the group it was given, that a node with nothing configured produces a
// process indistinguishable from the pre-existing one, and that the teardown
// removes what the create made.
//
// The tree is a directory the test builds, as in cgroup_test.go, so the assertions
// hold without root or a real cgroupfs. What that cannot show is the kernel
// enforcing the limit -- see the end of this file.

// TestStartVMMPutsThePidInItsCgroup is the test that goes red if the VMM stops
// being placed in its group. Without this the group exists, its limits are
// written, the startup log says limits are on, and the VMM is completely
// unbounded -- which is the same shape as the A3 documentation error, and worse,
// because somebody raises --overcommit-memory on the strength of it.
func TestStartVMMPutsThePidInItsCgroup(t *testing.T) {
	dir := t.TempDir()
	bin, pidFile, _ := stubVMM(t, dir)

	root := fakeV1Tree(t)
	h := newCgroupHost(root, cgroupV1)
	rt := &FCRuntime{FirecrackerBin: bin, Cgroups: h}

	spec := &Spec{SandboxID: "sb-cg", CPU: 1, MemoryMiB: 512}
	cg, err := h.createCgroup(spec.SandboxID, limitsFor(spec))
	if err != nil {
		t.Fatal(err)
	}
	vm := &fcVM{id: spec.SandboxID, dir: dir, cgroup: cg, done: make(chan struct{})}

	if err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock")); err != nil {
		t.Fatalf("startVMM: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL) })

	// The pid the stub reports, not the one Go recorded: they are the same only
	// because nothing wraps the command, and that property is what killVMM depends
	// on. Checking against the stub's own view keeps this test honest if a wrapper
	// is ever introduced.
	want := waitFile(t, pidFile)
	for _, c := range cgroupControllers {
		procs := filepath.Join(root, c, "bean-sb-cg", "cgroup.procs")
		b, err := os.ReadFile(procs)
		if err != nil {
			t.Errorf("%s: %v: the VMM was never added to its %s group, so that "+
				"limit does not apply to it", procs, err, c)
			continue
		}
		if got := strings.TrimSpace(string(b)); got != want {
			t.Errorf("%s holds %q, want the VMM's pid %q", procs, got, want)
		}
	}
	if strconv.Itoa(vm.cmd.Process.Pid) != want {
		t.Errorf("vm.cmd pid is %d but the VMM reports %s", vm.cmd.Process.Pid, want)
	}
}

// TestStartVMMWithoutCgroupsIsUnchanged is the untouched path, and it is the one
// that matters most: a node with none of this configured has to behave exactly as
// it did before, because that is what every existing deployment is running.
//
// Nothing is created anywhere, the process is the VMM's own and leads its group,
// and no SysProcAttr field beyond Setpgid is set -- a Credential written over that
// would produce a destroy that reports success while the microVM keeps running.
func TestStartVMMWithoutCgroupsIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	bin, pidFile, _ := stubVMM(t, dir)
	// Nil on both, which is what an unconfigured node has.
	rt := &FCRuntime{FirecrackerBin: bin}
	vm := &fcVM{id: "sb-plain", dir: dir, done: make(chan struct{})}

	if err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock")); err != nil {
		t.Fatalf("startVMM with nothing configured: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL) })

	if vm.cgroup != nil {
		t.Errorf("a cgroup was created on an unconfigured node: %+v", vm.cgroup)
	}
	if vm.cmd.SysProcAttr == nil || !vm.cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid was lost; killVMM signals the negative pid and needs the " +
			"VMM to lead its own process group")
	}
	if vm.cmd.SysProcAttr.Credential != nil {
		t.Errorf("credentials were applied with no uid configured: %+v",
			vm.cmd.SysProcAttr.Credential)
	}
	if got, want := strconv.Itoa(vm.cmd.Process.Pid), waitFile(t, pidFile); got != want {
		t.Errorf("recorded pid %s is not the VMM's %s", got, want)
	}
	if pgid, err := syscall.Getpgid(vm.cmd.Process.Pid); err != nil {
		t.Errorf("getpgid: %v", err)
	} else if pgid != vm.cmd.Process.Pid {
		t.Errorf("pgid is %d, want %d", pgid, vm.cmd.Process.Pid)
	}
}

// TestApplyCredsKeepsSetpgid covers the same collision directly, for the case
// where a uid *is* configured. The Credential must be added to the SysProcAttr the
// launch built, not written over it.
func TestApplyCredsKeepsSetpgid(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	applyCreds(cmd, &vmmCreds{UID: 1000, GID: 1000, Groups: []uint32{1000, 104}})

	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid was cleared by the credential drop: killVMM's kill(-pid) " +
			"would signal the wrong group, and a destroy would report success while " +
			"the microVM kept running")
	}
	if cmd.SysProcAttr.Credential == nil {
		t.Fatal("no credential was applied")
	}
	if got := cmd.SysProcAttr.Credential.Groups; len(got) != 2 || got[1] != 104 {
		t.Errorf("Groups = %v, want the kvm group carried through", got)
	}
}

// TestDestroyRemovesTheCgroup is the teardown assertion at the runtime level. A
// leaked cgroup directory is the GitHub #16 class of bug: nothing else on the host
// knows it exists, and it stays until the host reboots.
func TestDestroyRemovesTheCgroup(t *testing.T) {
	dir := t.TempDir()
	bin, _, _ := stubVMM(t, dir)

	root := fakeV1Tree(t)
	h := newCgroupHost(root, cgroupV1)
	rt := &FCRuntime{FirecrackerBin: bin, Cgroups: h, vms: map[string]*fcVM{}}

	cg, err := h.createCgroup("sb-destroy", limitsFor(&Spec{SandboxID: "sb-destroy", MemoryMiB: 256}))
	if err != nil {
		t.Fatal(err)
	}
	created := append([]string(nil), cg.dirs...)
	vm := &fcVM{id: "sb-destroy", dir: dir, cgroup: cg, done: make(chan struct{})}
	if err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock")); err != nil {
		t.Fatal(err)
	}
	rt.vms["sb-destroy"] = vm

	// rootfs is nil, which Destroy tolerates: Rootfs.Release is nil-safe. The
	// state directory removal and the cgroup teardown are what is under test.
	if err := rt.Destroy(t.Context(), "sb-destroy", true); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, d := range created {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s survived Destroy: an orphaned cgroup is invisible to every "+
				"other subsystem and stays for the life of the host", d)
		}
	}
}

// What none of the above establishes, stated so it is not mistaken for covered:
//
//   - That the kernel enforces the limits. The filenames and values are asserted
//     against a directory, so a value the kernel would reject (a quota outside its
//     accepted range, a memory limit below the group's current usage) passes here
//     and fails on a real host.
//   - That a dropped uid can actually run Firecracker: /dev/kvm, the dm device, the
//     vsock and UFFD sockets and the snapshot cache are each handled above, but
//     "handled" means the ownership and mode were set, not that a VMM booted a
//     guest through them. That needs a KVM host.
//   - The UFFD case specifically fails as a hang rather than an error, so its
//     absence is not visible as a failed test anywhere. uffdHandler.Faults() is
//     what distinguishes "the guest never faulted" from "the handler never
//     answered", and only a real restore exercises it.
