package beand

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestListenUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ln")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sock := filepath.Join(dir, "agent.sock")

	lis, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
}

// TestListenCreatesSocketDirectory covers the guest's first boot, where the
// parent directory does not exist yet.
func TestListenCreatesSocketDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "ln")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sock := filepath.Join(dir, "nested", "agent.sock")

	lis, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("socket not created: %v", err)
	}
}

// TestListenReplacesStaleSocket matters after an unclean shutdown: a leftover
// socket file would otherwise make every subsequent start fail.
func TestListenReplacesStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ln")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sock := filepath.Join(dir, "agent.sock")

	first, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	// Close without removing the file, as a killed process would leave it.
	if unixLis, ok := first.(*net.UnixListener); ok {
		unixLis.SetUnlinkOnClose(false)
	}
	first.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Skip("platform removed the socket on close; nothing stale to replace")
	}

	second, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	second.Close()
}

func TestListenRejectsMalformedVsockAddress(t *testing.T) {
	for _, addr := range []string{"vsock:", "vsock:0", "vsock:abc", "vsock:99999999999"} {
		if lis, err := Listen(addr); err == nil {
			lis.Close()
			t.Errorf("Listen(%q) accepted a malformed vsock address", addr)
		}
	}
}

// TestListenVsockRequiresLinux documents the platform split rather than
// leaving a darwin developer to discover it as a confusing bind error.
func TestListenVsockRequiresLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("vsock is available on linux; covered by the microVM e2e")
	}
	_, err := Listen("vsock:1024")
	if err == nil {
		t.Fatal("vsock listen succeeded on a platform without AF_VSOCK")
	}
	if !strings.Contains(err.Error(), "linux") {
		t.Errorf("error = %v, want it to name the platform requirement", err)
	}
}

// TestListenRefusesAnUnknownSchemeRatherThanCreatingAFile is a regression test for a
// failure that reported success.
//
// The agent image ships separately from noded, so a node can boot an agent older than
// itself. When that happened -- noded passing "tcp:0.0.0.0:10001" to an agent built
// before tcp: existed -- the address fell through to the Unix-socket branch and the
// agent created a socket named literally `tcp:0.0.0.0:10001`, logged "listening on
// tcp:0.0.0.0:10001", and served nothing anything could reach. The only symptom was a
// connection refused from noded twenty seconds later.
//
// Verified against the real stock disk before fixing: the file was created, srwxr-xr-x.
func TestListenRefusesAnUnknownSchemeRatherThanCreatingAFile(t *testing.T) {
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	for _, addr := range []string{
		"quic:0.0.0.0:10001", // a scheme that does not exist yet
		"tcp/0.0.0.0:10001",  // a plausible typo
		"agent.sock",         // a relative path
		"",
	} {
		lis, err := Listen(addr)
		if err == nil {
			lis.Close()
			t.Errorf("Listen(%q) succeeded; an address this agent cannot serve must be "+
				"an error rather than a file, because an agent listening on an "+
				"unreachable socket reports success and fails later, in noded", addr)
			continue
		}
		if !strings.Contains(err.Error(), "unusable listen address") {
			t.Errorf("Listen(%q) failed with %v, want the address named", addr, err)
		}
	}

	// And nothing was created on the way to those errors: the original bug was
	// precisely this file existing and looking like a working listener.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("Listen created %q while rejecting an address", e.Name())
	}
}

func TestListenTCPIsReachableOnTCP(t *testing.T) {
	// Port 0 so this does not depend on a free port. The network is what is asserted,
	// since the bug above was an address accepted onto the wrong one.
	lis, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer lis.Close()

	if got := lis.Addr().Network(); got != "tcp" {
		t.Fatalf("listener network is %q, want tcp", got)
	}
	// Reachable, not merely bound.
	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial the agent's own listener: %v", err)
	}
	conn.Close()
}

func TestListenRejectsMalformedTCPAddress(t *testing.T) {
	for _, addr := range []string{"tcp:", "tcp:notaport", "tcp:1.2.3.4"} {
		if lis, err := Listen(addr); err == nil {
			lis.Close()
			t.Errorf("Listen(%q) succeeded", addr)
		}
	}
}
