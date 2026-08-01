package beand

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Listen binds the agent's listener from an address string, so the same binary
// serves both tiers: a Unix socket when the sandbox is a host process, an
// AF_VSOCK port when it is a microVM guest.
//
// The protocol above this is identical either way — that is what lets the node
// run the same agent tests against both transports.
func Listen(addr string) (net.Listener, error) {
	if port, ok := strings.CutPrefix(addr, "vsock:"); ok {
		n, err := strconv.ParseUint(port, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("beand: vsock port %q: %w", port, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("beand: vsock port must be non-zero")
		}
		return listenVsock(uint32(n))
	}

	// A stale socket from a previous run would make bind fail; the sandbox
	// owns this path, so removing it is safe.
	if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
		return nil, fmt.Errorf("beand: create socket dir: %w", err)
	}
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("beand: remove stale socket: %w", err)
	}
	return net.Listen("unix", addr)
}
