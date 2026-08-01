// Package vsock dials the AF_VSOCK addresses microVM agents listen on.
//
// A microVM has no network the host can reach before the guest configures one,
// and giving every sandbox a tap device just to talk to its agent would make
// the control path depend on host networking. vsock is a host/guest channel
// that exists as soon as the VM boots, so the agent is reachable during early
// boot and stays reachable if guest networking is broken or absent.
//
// Firecracker exposes it as a Unix socket on the host: connecting and sending
// "CONNECT <port>\n" attaches to the guest's listener on that port. That
// indirection is why this is a package rather than a net.Dial call.
package vsock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Scheme is the gRPC target scheme these addresses use.
const Scheme = "vsock"

// Addr identifies a guest listener behind a Firecracker vsock Unix socket.
type Addr struct {
	// SocketPath is the host-side Unix socket Firecracker created.
	SocketPath string
	// Port is the port the guest agent listens on.
	Port uint32
}

// String renders the address in the form ParseAddr accepts.
func (a Addr) String() string {
	return fmt.Sprintf("%s:%s:%d", Scheme, a.SocketPath, a.Port)
}

// Target renders a gRPC dial target for this address.
func (a Addr) Target() string { return a.String() }

// ParseAddr reads "vsock:/path/to/sock:port". The socket path may contain
// colons, so the port is taken from the last one.
func ParseAddr(s string) (Addr, error) {
	rest, ok := strings.CutPrefix(s, Scheme+":")
	if !ok {
		return Addr{}, fmt.Errorf("vsock: address %q missing %q prefix", s, Scheme+":")
	}
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 || idx == len(rest)-1 {
		return Addr{}, fmt.Errorf("vsock: address %q must be vsock:<socket>:<port>", s)
	}
	port, err := strconv.ParseUint(rest[idx+1:], 10, 32)
	if err != nil {
		return Addr{}, fmt.Errorf("vsock: port in %q: %w", s, err)
	}
	if port == 0 {
		return Addr{}, fmt.Errorf("vsock: address %q has port 0", s)
	}
	return Addr{SocketPath: rest[:idx], Port: uint32(port)}, nil
}

// Dial connects to a guest listener. The handshake is synchronous: the host
// sends CONNECT and Firecracker answers OK before the connection carries
// application bytes, so a failure to reach the guest surfaces here rather than
// as an unexplained protocol error later.
func Dial(ctx context.Context, addr Addr) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", addr.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("vsock: dial %s: %w", addr.SocketPath, err)
	}

	// The deadline covers the handshake only; the caller governs the
	// connection's lifetime afterwards.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", addr.Port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock: send CONNECT: %w", err)
	}

	// Firecracker replies "OK <assigned_port>\n" on success. Reading it with a
	// bufio.Reader would buffer past the newline and swallow the first bytes
	// the guest sends, so the reply is read a byte at a time.
	reply, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock: read CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(reply, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock: guest port %d unreachable: %q", addr.Port, reply)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// readLine reads up to a newline without over-reading, so the connection is
// positioned exactly at the start of the guest's data.
func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 64 {
		n, err := conn.Read(buf)
		if err != nil {
			return b.String(), err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			return strings.TrimSuffix(b.String(), "\r"), nil
		}
		b.WriteByte(buf[0])
	}
	return b.String(), errors.New("reply too long")
}
