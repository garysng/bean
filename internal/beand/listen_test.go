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
