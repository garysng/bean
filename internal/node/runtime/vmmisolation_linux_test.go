//go:build linux

package runtime

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These assert on a live process rather than on the SysProcAttr struct.
//
// The distinction is the whole point: clone flags either took effect during the fork
// or the process is not isolated, and there is no later moment at which that becomes
// visible. A test that checks the struct confirms the code set a field, which is not
// the claim being made.

func TestPidNamespaceIsolatesTheProcess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("CLONE_NEWPID needs root")
	}

	// sleep is enough: what matters is which namespace it lands in, not what it does.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	isolateVMM(cmd, VMMIsolation{PIDNamespace: true})
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Compared by inode, because two namespaces can only be told apart by identity --
	// a string comparison of paths would pass whatever the kernel did.
	hostNS, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatalf("read this process's pid namespace: %v", err)
	}
	childNS, err := os.Readlink("/proc/" + itoa(cmd.Process.Pid) + "/ns/pid")
	if err != nil {
		t.Fatalf("read the child's pid namespace: %v", err)
	}
	if hostNS == childNS {
		t.Fatalf("child is in the host's pid namespace (%s); it can see and signal "+
			"every process on the node", hostNS)
	}

	// And it really is PID 1 in there, which is what makes the SIGKILL choice in
	// isolateVMM load-bearing rather than stylistic.
	status, err := os.ReadFile("/proc/" + itoa(cmd.Process.Pid) + "/status")
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(string(status), "NSpid:") {
		t.Skip("kernel does not report NSpid")
	}
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		f := strings.Fields(line)
		// NSpid lists the pid in each namespace, outermost first.
		if len(f) < 3 || f[len(f)-1] != "1" {
			t.Errorf("NSpid is %q; the VMM should be pid 1 inside its own namespace, "+
				"which is why isolateVMM uses SIGKILL and not SIGTERM", line)
		}
	}
}

func TestTheRecordedPidIsTheProcessItself(t *testing.T) {
	// The reason this uses clone flags rather than exec'ing `unshare`: with a wrapper,
	// cmd.Process.Pid names the wrapper, and whether killing it reaches the VMM
	// depends on whether each layer execs in place or forks. The failure is a destroy
	// that reports success while the microVM keeps running.
	if os.Geteuid() != 0 {
		t.Skip("CLONE_NEWPID needs root")
	}

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	isolateVMM(cmd, VMMIsolation{PIDNamespace: true, KillOnNodedDeath: true})
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The recorded pid must name a process whose command line is the one asked for,
	// not a wrapper's.
	//
	// Polled rather than read once. An empty cmdline does not mean "a wrapper": it
	// means the pid is a zombie, exited but not yet reaped, and a read immediately
	// after Start can land before exec has replaced the forked child's image. Reading
	// once made this fail only when another test ran first, which looked like
	// interference between the flags and was not.
	got := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cmdline, err := os.ReadFile("/proc/" + itoa(cmd.Process.Pid) + "/cmdline")
		if err == nil && len(cmdline) > 0 {
			got = strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(got, "sleep") {
		t.Fatalf("recorded pid %d is %q, not the command that was started; a wrapper "+
			"process means killing this pid may not reach the VMM",
			cmd.Process.Pid, got)
	}

	// Signalling the negative pid must reach it, which is what destroy does.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the process group: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the process survived a SIGKILL to its process group, which is what " +
			"destroy sends")
	}
}

func TestSetpgidAndCredentialSurviveIsolation(t *testing.T) {
	// isolateVMM adds to whatever SysProcAttr the caller built. Replacing it would
	// drop Setpgid, which killVMM's negative pid depends on, and Credential, which is
	// the uid drop -- both silently, and both only visible much later.
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: 1234, Gid: 5678},
	}
	isolateVMM(cmd, VMMIsolation{PIDNamespace: true, MountNamespace: true,
		KillOnNodedDeath: true})

	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid was dropped; killVMM signals the negative pid and depends on " +
			"the VMM leading its own group")
	}
	if cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 1234 {
		t.Error("Credential was dropped; the VMM would run as root")
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWPID == 0 {
		t.Error("CLONE_NEWPID not set")
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWNS == 0 {
		t.Error("CLONE_NEWNS not set")
	}
	if cmd.SysProcAttr.Unshareflags&syscall.CLONE_NEWNS == 0 {
		t.Error("Unshareflags lacks CLONE_NEWNS; without it the new mount namespace " +
			"keeps shared propagation and mounts made inside travel back to the host")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig is %v, want SIGKILL: in a pid namespace the VMM is pid 1, "+
			"and pid 1 ignores signals it has no handler for", cmd.SysProcAttr.Pdeathsig)
	}
	// CLONE_NEWNET must never appear: it would make a new empty namespace, while this
	// sandbox's namespace already exists with its tap in it.
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWNET != 0 {
		t.Error("CLONE_NEWNET is set; that creates an empty namespace rather than " +
			"joining the sandbox's, so the guest would have no tap")
	}
}

func TestZeroValueAppliesNothing(t *testing.T) {
	// Every deployment before this ran with no isolation, and must keep running
	// identically when the flags are not set.
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	isolateVMM(cmd, VMMIsolation{})

	if cmd.SysProcAttr.Cloneflags != 0 {
		t.Errorf("Cloneflags = %#x for the zero value, want 0", cmd.SysProcAttr.Cloneflags)
	}
	if cmd.SysProcAttr.Unshareflags != 0 {
		t.Errorf("Unshareflags = %#x for the zero value, want 0", cmd.SysProcAttr.Unshareflags)
	}
	if cmd.SysProcAttr.Pdeathsig != 0 {
		t.Errorf("Pdeathsig = %v for the zero value, want none", cmd.SysProcAttr.Pdeathsig)
	}
}

func TestSummaryNamesTheAbsenceOfIsolation(t *testing.T) {
	// A log line that only lists what is on reads identically on a node with no
	// isolation and on one where the flags were forgotten.
	if s := (VMMIsolation{}).Summary(); !strings.Contains(s, "no isolation") {
		t.Errorf("zero-value summary is %q; it should say plainly that nothing is on", s)
	}
	if s := (VMMIsolation{PIDNamespace: true}).Summary(); !strings.Contains(s, "pid") {
		t.Errorf("summary %q does not mention the pid namespace", s)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
