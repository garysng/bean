//go:build !linux

package beand

import (
	"fmt"
	"net"
)

// listenVsock reports that vsock is unavailable. The agent still builds and
// runs on darwin for development, where it is reached over a Unix socket; only
// the microVM tier needs vsock, and that tier is Linux-only anyway.
func listenVsock(port uint32) (net.Listener, error) {
	return nil, fmt.Errorf("beand: vsock requires linux (asked for port %d)", port)
}

// isVsockListener is always false off Linux, where there is no vsock listener.
func isVsockListener(net.Listener) bool { return false }
