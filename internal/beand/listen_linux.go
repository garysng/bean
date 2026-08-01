//go:build linux

package beand

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// listenVsock binds an AF_VSOCK listener on the given port.
//
// The socket is created with raw syscalls because net.Listen has no vsock
// support, and the descriptor cannot be handed to net.FileListener either: the
// standard library calls getsockname to classify the socket and AF_VSOCK is not
// a family it knows, so adoption fails outright. The accept loop is therefore
// implemented here.
//
// VMADDR_CID_ANY accepts from the host without the guest needing to know its
// own context id, which it cannot rely on before the virtio driver has settled.
func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("beand: vsock socket: %w", err)
	}

	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("beand: bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("beand: listen vsock port %d: %w", port, err)
	}
	return &vsockListener{fd: fd, port: port}, nil
}

// vsockListener implements net.Listener over a raw AF_VSOCK descriptor.
type vsockListener struct {
	fd   int
	port uint32

	mu     sync.Mutex
	closed bool
}

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		nfd, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)

		// Close's wake-up connection arrives as an ordinary accept, so the
		// closed flag is checked either way. Reporting the connection instead
		// would hand the caller a peer that immediately disappears.
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			if err == nil {
				unix.Close(nfd)
			}
			return nil, net.ErrClosed
		}

		if err != nil {
			// Accept is interrupted by any signal the runtime delivers, which
			// is routine rather than a failure.
			if err == unix.EINTR {
				continue
			}
			return nil, &net.OpError{Op: "accept", Net: "vsock", Err: err}
		}
		return newVsockConn(nfd, l.localAddr(), remoteAddr(sa)), nil
	}
}

func (l *vsockListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	// shutdown does not wake a blocked accept on a listening vsock socket, so
	// a self-connection is used to make accept return. Without it Close would
	// leave the accept goroutine parked and the agent would never exit.
	l.wake()
	return unix.Close(l.fd)
}

// wake connects to our own listening port so a blocked Accept returns. It runs
// on a best-effort basis: if the guest cannot reach its own port, closing the
// descriptor is still the right thing to do.
func (l *vsockListener) wake() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	// VMADDR_CID_LOCAL loops back within this VM, which is what makes the
	// connection reach our own listener rather than the host's.
	_ = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: l.port})
}

func (l *vsockListener) Addr() net.Addr { return l.localAddr() }

func (l *vsockListener) localAddr() net.Addr {
	return vsockAddr{cid: unix.VMADDR_CID_ANY, port: l.port}
}

func remoteAddr(sa unix.Sockaddr) net.Addr {
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		return vsockAddr{cid: vm.CID, port: vm.Port}
	}
	return vsockAddr{}
}

// vsockAddr renders a vsock endpoint for logs and error messages.
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock:%d:%d", a.cid, a.port) }

// vsockConn wraps an accepted descriptor. The file-backed connection gives
// deadline support and integration with the runtime poller, which raw
// read/write syscalls would not have — gRPC needs both.
type vsockConn struct {
	*os.File
	local  net.Addr
	remote net.Addr
}

func newVsockConn(fd int, local, remote net.Addr) net.Conn {
	return &vsockConn{
		File:   os.NewFile(uintptr(fd), remote.String()),
		local:  local,
		remote: remote,
	}
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }
