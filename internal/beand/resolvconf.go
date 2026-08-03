package beand

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// resolvConfPath is where libc looks for resolver configuration. It is not
// configurable because glibc and musl both hardcode it.
const resolvConfPath = "/etc/resolv.conf"

// ValidateResolver rejects an address that cannot serve as a guest's nameserver.
//
// The one case that matters is loopback. A host running systemd-resolved has
// 127.0.0.53 in its own /etc/resolv.conf, and that address is meaningful only on
// the host: inside a guest it names the guest itself, where nothing is
// listening. Passing it through produces a sandbox whose every lookup times out
// while every layer below DNS -- route, NAT, ping to a literal address -- tests
// clean, which sends people looking at the network stack for a problem that is
// one line in a file. So a loopback resolver is refused at the point it enters
// configuration rather than written into a guest.
//
// An unparseable address is refused in the same breath because resolv.conf takes
// literal addresses only; a hostname there is silently ignored by libc, which is
// the same undebuggable outcome by a different route.
func ValidateResolver(addr string) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("resolver %q is not an IP address; /etc/resolv.conf takes literal addresses", addr)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("resolver %s is loopback, which from inside a guest points at the guest itself; "+
			"give the upstream resolver the host forwards to", addr)
	}
	return nil
}

// WriteResolvConf points the guest's libc at a resolver that exists.
//
// A user image can carry any /etc/resolv.conf its builder happened to leave
// behind, so the file is replaced rather than merged: keeping unknown entries
// means keeping whatever unreachable address the image inherited from its build
// machine, and libc will spend its full timeout on that before trying anything
// else.
//
// root confines the write for the dev tier, where the sandbox is a host process
// rather than a guest; "" means the real root, which is the microVM case.
//
// Writing is idempotent by construction -- the file is truncated to a value
// derived only from configuration -- which is what makes it safe on a sandbox
// restored from a snapshot that already has the file.
func WriteResolvConf(root, addr string) error {
	if err := ValidateResolver(addr); err != nil {
		return fmt.Errorf("beand: %w", err)
	}

	path := resolvConfPath
	if root != "" {
		path = filepath.Join(root, resolvConfPath)
	}

	// A minimal image may have no /etc at all, and a sandbox with no name
	// resolution is a worse outcome than a directory the image did not ask for.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("beand: create %s: %w", filepath.Dir(path), err)
	}

	// Removed before being created because distributions ship this path as a
	// symlink into /run (systemd-resolved's stub) and /run in a guest is a tmpfs
	// this agent mounted moments ago and is therefore empty. Writing through the
	// symlink would either fail on the missing directory or deposit the file
	// somewhere libc never reads. Replacing the link with a regular file is the
	// only outcome that resolves.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("beand: replace %s: %w", path, err)
	}

	content := "nameserver " + addr + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("beand: write %s: %w", path, err)
	}
	return nil
}
