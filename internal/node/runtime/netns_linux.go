//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// Joining the sandbox's network namespace.
//
// internal/node/network creates one namespace per sandbox and puts the tap
// inside it (setup_linux.go, "ip netns exec <ns> ip tuntap add"). The VMM has to
// be in that namespace or the device name it is handed does not resolve: the
// tap is not a path, it is a name looked up in the caller's netns. Launching
// Firecracker from the host namespace and then telling it to attach to
// "beantap0" asks it for a device that does not exist there, and the whole
// networking stack is dead while every request-level assertion still passes.

// netnsHandleDir is where "ip netns add" leaves a bind-mounted handle to the
// namespace it created. The network package creates namespaces with that
// command, so this is where their handles are.
//
// It is also exactly what Firecracker's jailer takes as --netns: jailer opens
// the path and calls setns on it, it never creates a namespace. So the choice of
// identifying a namespace by this path rather than by a pid is what lets GitHub
// #20 phase 2 hand the same string to jailer and delete the code below.
const netnsHandleDir = "/var/run/netns"

// netnsPathFor returns the handle of the namespace the sandbox's VMM belongs in,
// or "" when this node has no networking configured.
//
// The empty string is a supported answer, not a failure: a node without a
// network pool boots sandboxes with no NIC at all, and that path has to keep
// working unchanged.
func netnsPathFor(spec *Spec) string {
	if spec == nil || spec.Network == nil || spec.Network.Netns == "" {
		return ""
	}
	return filepath.Join(netnsHandleDir, spec.Network.Netns)
}

// The vsock and UFFD sockets are unaffected by this, which was checked rather
// than assumed because a wrong answer would present as a restore that hangs
// forever on a page fault nobody answers. A pathname AF_UNIX socket is resolved
// through the filesystem and is not network-namespaced: a process inside a fresh
// netns connects to a socket bound in the host netns and is served normally
// (measured with socat across "ip netns exec"). That is the exact direction here
// -- noded binds uffd.sock and the namespaced Firecracker connects inward -- so
// both sockets keep working. An abstract socket (a name starting with NUL) would
// not: those live in the network namespace, and neither socket here uses one.

// startInNetns starts cmd inside the network namespace at nsPath, or in the
// caller's namespace when nsPath is empty.
//
// Everything about cmd is left to the caller: cmd.Dir still decides the working
// directory (snapshot portability depends on it being the sandbox directory, see
// vm-assembly.md section 5, and entering a netns does not change the cwd -- this
// is measured in hack/netns-cwd-probe.sh and recorded in network.md section 1),
// and cmd.Stdout/cmd.Stderr are inherited as ordinary fds because this is a
// plain fork/exec of the caller's own command.
//
// The process started is cmd itself. There is no wrapper binary between the
// caller and Firecracker, so cmd.Process.Pid is the VMM's pid and the negative
// pid that killVMM sends SIGTERM to still names Firecracker's process group.
// That is the reason this is a setns and not "ip netns exec firecracker": with a
// wrapper, the recorded pid is the wrapper's, and whether killing it reaches the
// VMM depends on whether the wrapper execs in place or forks. The visible
// failure would be a destroy that returns success while the microVM keeps
// running and keeps holding the memory the scheduler has already handed out.
func startInNetns(cmd *exec.Cmd, nsPath string) error {
	if nsPath == "" {
		// No namespace to join: start on the caller's thread exactly as the
		// pre-networking code did. No setns, no extra goroutine, no wrapper. A
		// node with nil Layout must produce an identical process tree, because
		// that is the configuration every existing deployment is running.
		return cmd.Start()
	}

	// Opened before any thread is pinned: a missing handle is the common
	// operational failure (namespace torn down under us, or noded restarted
	// against a host that was swept) and it should surface as this error rather
	// than as a VM that boots into the host namespace with no working NIC.
	// Failing closed is deliberate. A sandbox that comes up with no network looks
	// healthy and fails much later inside the guest, as pip and git timing out.
	target, err := os.Open(nsPath)
	if err != nil {
		return fmt.Errorf("open network namespace %s: %w", nsPath, err)
	}
	defer target.Close()

	// setns(2) changes one thread's namespace, and the Go runtime moves
	// goroutines between threads at every blocking point. A setns followed by
	// cmd.Start() on the same goroutine is therefore not enough: the goroutine
	// can be resumed on a different thread and fork the VMM straight back into
	// the host namespace. That failure is intermittent, which is worse than the
	// bug being fixed. The join and the fork have to happen on one thread that
	// stays pinned across both, so both run in a goroutine of their own that
	// holds the thread for the whole window.
	//
	// clone(CLONE_NEWNET) is not an option here even though it would avoid the
	// pinning: it makes a new empty namespace, and this needs the existing one
	// that holds the tap.
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()

		// Not deferred, and not always called. If the restore below fails the
		// thread is still in the sandbox's namespace, and unlocking it would
		// return it to the runtime's pool, where the next goroutine to land on
		// it would silently get sandbox networking for whatever it does next --
		// including the next sandbox's setup commands. Leaving the goroutine
		// while the thread is still locked makes the runtime destroy the thread
		// instead, which is the only safe way to dispose of one.
		unlock := false
		defer func() {
			if unlock {
				runtime.UnlockOSThread()
			}
		}()

		// thread-self rather than self: the namespace being saved and restored
		// belongs to this thread, and /proc/self/ns/net is the namespace of the
		// process' main thread, which is a different one.
		host, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			errc <- fmt.Errorf("open current network namespace: %w", err)
			unlock = true
			return
		}
		defer host.Close()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- fmt.Errorf("enter network namespace %s: %w", nsPath, err)
			unlock = true
			return
		}

		startErr := cmd.Start()

		// The child has been forked by now and carries the namespace, so the
		// restore only concerns this thread.
		if err := unix.Setns(int(host.Fd()), unix.CLONE_NEWNET); err != nil {
			// unlock stays false: see above. The started process is reported as
			// successful if it started, because it exists and the caller has to
			// be told about it or it leaks.
			if startErr != nil {
				errc <- startErr
				return
			}
			errc <- nil
			return
		}
		unlock = true
		errc <- startErr
	}()
	return <-errc
}
