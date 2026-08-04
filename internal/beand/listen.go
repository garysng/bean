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

	// A TCP address, which is what makes one addressing scheme cover both the agent
	// and any port a user exposes: a proxy in front resolves {port}-{sandbox} to a
	// port inside the guest and does not need to know that one of those ports is the
	// agent's.
	//
	// It is also what gives up the isolation vsock provided for free -- a process
	// inside the sandbox can now dial this -- which is why Authenticator gates every
	// method and why a hash is published through the metadata service before the
	// guest starts. Serving this without that check would put an unauthenticated
	// root-equivalent API inside every sandbox.
	if hostPort, ok := strings.CutPrefix(addr, "tcp:"); ok {
		if _, _, err := net.SplitHostPort(hostPort); err != nil {
			return nil, fmt.Errorf("beand: tcp address %q: %w", hostPort, err)
		}
		return net.Listen("tcp", hostPort)
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
