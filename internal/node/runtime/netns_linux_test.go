//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/garysng/bean/internal/node/network"
)

// What these tests are guarding is which network namespace the VMM process ends
// up in, and that is not visible to the fcRecorder the other tests in this
// package use: the recorder sees the NIC registration request, and that request
// looks identical whether or not the tap it names exists anywhere. The whole
// original bug was invisible for exactly that reason.
//
// Two levels are used. The kernel outcome is asserted by comparing
// /proc/<pid>/ns/net against the namespace handle, which needs root and so
// skips. The intent is asserted without root by giving startVMM a namespace that
// cannot be joined and requiring it to fail: a launch that ignores the namespace
// has nothing to fail on, so dropping the join turns that test red on any Linux
// box. Neither substitutes for the other -- the second checks that the value is
// consulted, not that the kernel honoured it.

// stubVMM writes a fake firecracker to dir. It records its own pid and the
// inode of its network namespace, which is what makes the process' namespace and
// identity observable to the test without a real VMM or KVM.
func stubVMM(t *testing.T, dir string) (bin, pidFile, nsFile string) {
	t.Helper()
	bin = filepath.Join(dir, "fake-firecracker")
	pidFile = filepath.Join(dir, "pid")
	nsFile = filepath.Join(dir, "ns")
	script := fmt.Sprintf(`#!/bin/sh
echo $$ > %q
readlink /proc/self/ns/net > %q
sleep 30
`, pidFile, nsFile)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, pidFile, nsFile
}

// waitFile waits for the stub to have written a file, so the test is not racing
// the process it just started.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s was never written by the stub VMM", path)
	return ""
}

func TestNetnsPathForUsesTheSandboxNamespace(t *testing.T) {
	layout, err := network.LayoutFor(7, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	got := netnsPathFor(&Spec{SandboxID: "sb", Network: layout})
	want := filepath.Join("/var/run/netns", layout.Netns)
	if got != want {
		t.Errorf("netnsPathFor = %q, want %q; this is the path form jailer's "+
			"--netns also takes, so it must stay a handle under %s",
			got, want, netnsHandleDir)
	}
}

// TestNetnsPathForWithoutNetworkIsEmpty pins the untouched path. A node with no
// network pool has to launch exactly as it did before namespaces existed.
func TestNetnsPathForWithoutNetworkIsEmpty(t *testing.T) {
	for name, spec := range map[string]*Spec{
		"nil spec":    nil,
		"nil layout":  {SandboxID: "sb"},
		"empty netns": {SandboxID: "sb", Network: &network.Layout{}},
	} {
		if got := netnsPathFor(spec); got != "" {
			t.Errorf("%s: netnsPathFor = %q, want empty so no namespace is joined",
				name, got)
		}
	}
}

// TestStartVMMWithoutNetworkStartsDirectly is the no-networking path: no
// namespace, and the recorded pid is the VMM's own and its group leader. If the
// join were ever implemented by wrapping the command in "ip netns exec", the pid
// here would be the wrapper's, and killVMM's kill(-pid) would signal the wrong
// group -- a destroy that reports success and leaves the microVM running.
func TestStartVMMWithoutNetworkStartsDirectly(t *testing.T) {
	dir := t.TempDir()
	bin, pidFile, _ := stubVMM(t, dir)
	rt := &FCRuntime{FirecrackerBin: bin}
	vm := &fcVM{id: "sb-nonet", dir: dir, done: make(chan struct{})}

	if err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock")); err != nil {
		t.Fatalf("startVMM with no network: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL) })

	reported := vm.cmd.Process.Pid
	actual := waitFile(t, pidFile)
	if strconv.Itoa(reported) != actual {
		t.Errorf("vm.cmd pid is %d but the VMM reports %s: the recorded pid is not "+
			"the VMM's, so killVMM would signal something else and Handle would "+
			"report a pid that is not the microVM", reported, actual)
	}
	// Firecracker is expected to lead its own group; killVMM relies on it.
	if pgid, err := syscall.Getpgid(reported); err != nil {
		t.Errorf("getpgid(%d): %v", reported, err)
	} else if pgid != reported {
		t.Errorf("pgid of the VMM is %d, want %d: kill(-pid) in killVMM only "+
			"reaches the guest's helpers if the VMM leads the group", pgid, reported)
	}
}

// TestStartVMMFailsWhenTheNamespaceCannotBeJoined is the test that goes red if
// the launch stops entering the namespace. It needs no root and no KVM.
//
// A layout is present, so the sandbox is meant to be in a namespace, and the
// handle for it does not exist. The only way to succeed here is to ignore the
// namespace and launch in the host's, which is precisely the bug: the VMM comes
// up, the NIC registration is accepted, and nothing reports a problem until pip
// and git fail inside the guest.
//
// It checks that the namespace is consulted, not that the kernel placed the
// process. TestStartVMMEntersTheSandboxNamespace does that, under root.
func TestStartVMMFailsWhenTheNamespaceCannotBeJoined(t *testing.T) {
	dir := t.TempDir()
	bin, _, _ := stubVMM(t, dir)
	rt := &FCRuntime{FirecrackerBin: bin}
	vm := &fcVM{
		id:  "sb-missing-ns",
		dir: dir,
		// A handle that is not there. Namespaces are torn down when a sandbox
		// goes away, so this is also the real race, not only a synthetic one.
		netnsPath: filepath.Join(dir, "no-such-netns"),
		done:      make(chan struct{}),
	}

	err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock"))
	if err == nil {
		if vm.cmd != nil && vm.cmd.Process != nil {
			_ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL)
		}
		t.Fatal("startVMM succeeded with an unjoinable network namespace: the VMM " +
			"was launched without entering it, so the tap the NIC names is not " +
			"visible to it and the sandbox has no network")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error does not name the namespace as the cause: %v", err)
	}
}

// TestStartInNetnsEmptyPathStartsInPlace covers the launcher directly for the
// no-network case: no namespace argument, ordinary start, fds and cwd untouched.
func TestStartInNetnsEmptyPathStartsInPlace(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cmd := exec.Command("/bin/sh", "-c", "pwd")
	cmd.Dir = dir
	cmd.Stdout = f
	if err := startInNetns(cmd, ""); err != nil {
		t.Fatalf("startInNetns with no namespace: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Both halves of the contract in one line of output: the fd was inherited
	// (there is output at all) and cmd.Dir was honoured (it is the sandbox dir).
	// Snapshot portability rests on the second -- every path Firecracker records
	// is relative to it.
	got := strings.TrimSpace(string(b))
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("child cwd = %q, want %q: cmd.Dir must still decide the working "+
			"directory, or a snapshot's relative paths resolve somewhere else", got, want)
	}
}

// TestStartVMMEntersTheSandboxNamespace is the kernel-level assertion: the VMM
// process' own view of its network namespace must be the sandbox's, not the
// host's. This is the one that cannot be satisfied by a launch that merely
// mentions the namespace.
//
// Root only, because creating a namespace needs it. On a developer machine and
// in a container without CAP_SYS_ADMIN it skips, so the test above is what
// guards the behaviour day to day.
func TestStartVMMEntersTheSandboxNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("creating a network namespace needs root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not available")
	}

	ns := "bean-test-" + strconv.Itoa(os.Getpid())
	if out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput(); err != nil {
		t.Skipf("ip netns add: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })

	nsPath := filepath.Join(netnsHandleDir, ns)
	wantNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	bin, pidFile, nsFile := stubVMM(t, dir)
	rt := &FCRuntime{FirecrackerBin: bin}
	vm := &fcVM{id: "sb-ns", dir: dir, netnsPath: nsPath, done: make(chan struct{})}

	if err := rt.startVMM(t.Context(), vm, filepath.Join(dir, "api.sock")); err != nil {
		t.Fatalf("startVMM: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-vm.cmd.Process.Pid, syscall.SIGKILL) })

	gotNS := waitFile(t, nsFile)
	if gotNS == wantNS {
		t.Errorf("the VMM is in the host network namespace %s: the tap lives in %s "+
			"and is addressed by name, so the device the NIC registration names "+
			"does not exist where the VMM is looking", gotNS, ns)
	}

	// And specifically the sandbox's namespace, not merely some other one.
	var st syscall.Stat_t
	if err := syscall.Stat(nsPath, &st); err != nil {
		t.Fatal(err)
	}
	wantInode := fmt.Sprintf("net:[%d]", st.Ino)
	if gotNS != wantInode {
		t.Errorf("VMM network namespace = %s, want the sandbox's %s", gotNS, wantInode)
	}

	// The pid must still be the VMM's after a namespace join, or destroy signals
	// the wrong process.
	if reported, actual := strconv.Itoa(vm.cmd.Process.Pid), waitFile(t, pidFile); reported != actual {
		t.Errorf("vm.cmd pid is %s but the VMM reports %s", reported, actual)
	}

	// The thread that did the join must not have been left in the namespace: it
	// goes back to the runtime's pool, and the next goroutine to run on it would
	// silently inherit sandbox networking.
	after, err := os.Readlink("/proc/thread-self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if after != wantNS {
		t.Errorf("caller is now in %s rather than the host namespace %s", after, wantNS)
	}
}
