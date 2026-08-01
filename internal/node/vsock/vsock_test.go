package vsock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSocketPath returns a socket path under the system temp root rather than
// t.TempDir(). Unix socket paths are limited to about 104 bytes, and on macOS
// t.TempDir() alone can exceed that once the test name is in it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	path := filepath.Join(dir, "v.sock")
	if len(path) > 100 {
		t.Skipf("socket path %q too long for this platform", path)
	}
	return path
}

func TestParseAddrRoundTrip(t *testing.T) {
	for _, want := range []Addr{
		{SocketPath: "/run/bean/sbx_1/vsock.sock", Port: 1024},
		{SocketPath: "/tmp/v", Port: 65535},
		// A socket path containing a colon must still parse: the port is
		// taken from the last separator, not the first.
		{SocketPath: "/tmp/odd:name/v.sock", Port: 7},
	} {
		got, err := ParseAddr(want.String())
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", want.String(), err)
		}
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	}
}

func TestParseAddrRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",
		"/run/v.sock:1024",           // no scheme
		"unix:/run/v.sock:1024",      // wrong scheme
		"vsock:/run/v.sock",          // no port
		"vsock:/run/v.sock:",         // empty port
		"vsock::1024",                // no socket path
		"vsock:/run/v.sock:0",        // port 0 is not a listener
		"vsock:/run/v.sock:notaport", // non-numeric
		"vsock:/run/v.sock:99999999999",
	} {
		if _, err := ParseAddr(s); err == nil {
			t.Errorf("ParseAddr(%q) accepted a malformed address", s)
		}
	}
}

// fakeFirecrackerVsock stands in for the host-side socket Firecracker creates.
// It speaks the CONNECT handshake, so the dialer is tested against the protocol
// rather than against a mock of itself.
func fakeFirecrackerVsock(t *testing.T, wantPort uint32, payload string) string {
	t.Helper()
	sockPath := shortSocketPath(t)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := readLine(conn)
				if err != nil {
					return
				}
				var port uint32
				if _, err := fmt.Sscanf(line, "CONNECT %d", &port); err != nil {
					fmt.Fprint(conn, "FAILED\n")
					return
				}
				if port != wantPort {
					// This is what Firecracker answers when no guest
					// listener is bound to the requested port.
					fmt.Fprint(conn, "FAILED\n")
					return
				}
				fmt.Fprintf(conn, "OK %d\n", 1000+port)
				fmt.Fprint(conn, payload)
			}()
		}
	}()
	return sockPath
}

// TestDialCompletesHandshakeAndPreservesPayload is the property the agent
// connection depends on: the CONNECT reply must be consumed exactly, with no
// over-read that would swallow the guest's first bytes.
func TestDialCompletesHandshakeAndPreservesPayload(t *testing.T) {
	const payload = "HTTP/2 preface would go here"
	sockPath := fakeFirecrackerVsock(t, 1024, payload)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, Addr{SocketPath: sockPath, Port: 1024})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("payload = %q, want %q", got, payload)
	}
}

// TestDialClearsHandshakeDeadline guards against the connection inheriting the
// dial deadline, which would break long-lived streams like logs or exec.
func TestDialClearsHandshakeDeadline(t *testing.T) {
	sockPath := fakeFirecrackerVsock(t, 1024, "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	conn, err := Dial(ctx, Addr{SocketPath: sockPath, Port: 1024})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Past the dial deadline, a read must report EOF from the peer rather
	// than a timeout from a stale deadline.
	time.Sleep(300 * time.Millisecond)
	_, err = conn.Read(make([]byte, 1))
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Error("connection kept the handshake deadline")
	}
}

func TestDialReportsUnreachableGuestPort(t *testing.T) {
	sockPath := fakeFirecrackerVsock(t, 1024, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, Addr{SocketPath: sockPath, Port: 9999})
	if err == nil {
		t.Fatal("dial to an unbound guest port succeeded")
	}
	// The error must name the port: an agent that failed to listen is a
	// different problem from a VM that never booted.
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("error = %v, want the port named", err)
	}
}

func TestDialReportsMissingSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, Addr{SocketPath: filepath.Join(t.TempDir(), "absent.sock"), Port: 1024})
	if err == nil {
		t.Fatal("dial to a missing socket succeeded")
	}
}

// TestDialHonoursContextCancellation matters because sandbox creation is
// cancellable: a dial must not outlive the request that started it.
func TestDialHonoursContextCancellation(t *testing.T) {
	// A listener that accepts but never answers the handshake.
	sockPath := shortSocketPath(t)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		conn, err := lis.Accept()
		if err == nil {
			// Hold the connection open without replying.
			time.Sleep(5 * time.Second)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, Addr{SocketPath: sockPath, Port: 1024}); err == nil {
		t.Fatal("dial succeeded despite no handshake reply")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("dial took %v, expected the context deadline to apply", elapsed)
	}
}

func TestReadLineRejectsOverlongReply(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		fmt.Fprint(server, strings.Repeat("x", 200))
	}()
	if _, err := readLine(client); err == nil {
		t.Error("readLine accepted a reply with no newline")
	}
}
