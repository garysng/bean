package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dropping the VMM's privileges, and the rlimits that bound it.
//
// Today Firecracker runs as root in the host's mount namespace. The hardware
// virtualisation boundary is still the main defence, but an FC or KVM bug behind
// it reaches host root rather than an unprivileged process. Dropping the uid is
// the part of that gap reachable without a chroot, and it is worth being exact
// about how much it buys: the process keeps a full *view* of the host filesystem,
// merely not write access to most of it. Confining what it can see needs a mount
// namespace, which is GitHub #20 phase 2 (docs/jailer.md sections 3 and 7).
//
// The uid is node configuration and the same for every sandbox on the node. Two
// consequences, both stated because neither is obvious:
//
//   - A VMM can reach another sandbox's directory on the same node, since they
//     share the uid that owns it. That is weaker than jailer, which gives each
//     jail its own root, and weaker than a per-sandbox uid would be.
//   - A per-sandbox uid was not chosen because it needs a uid range reserved on
//     the host and an allocator with the same reclaim problem as every other
//     per-sandbox resource here, for a boundary between processes that are each
//     already behind their own VM. It is a real improvement and it is deferred,
//     not dismissed.
//
// Nothing below runs unless an operator sets the uid. A node that has not is
// unaffected in every respect, which matters because the accessibility work here
// is the part most likely to be wrong on a host nobody has tested it on.

// vmmCreds is the identity a sandbox's VMM runs as.
//
// A nil *vmmCreds means "run as noded does", which is the pre-existing behaviour.
// Every method tolerates nil so the launch path has one shape rather than two.
type vmmCreds struct {
	// UID and GID are what the VMM process runs as.
	UID uint32
	GID uint32
	// Groups are supplementary groups, which is how /dev/kvm is reached without
	// changing its ownership: on every distribution checked it is root:kvm 0660,
	// shared with libvirt and anything else using KVM on the host, so chowning it
	// to bean's uid would be taking a host-wide device away from other users. The
	// group that already owns it is the right answer.
	Groups []uint32
	// NoFile caps open descriptors, NProc caps processes and threads. Zero leaves
	// either at the inherited value.
	NoFile uint64
	NProc  uint64
}

// Default rlimits for a VMM.
//
// Both are chosen to bound a VMM that has gone wrong rather than to be tight.
// Firecracker opens a handful of descriptors -- the API socket, the drives, the
// vsock, the memory backend -- and runs one thread per vCPU plus a few of its own,
// so a healthy VMM is orders of magnitude below either. A limit tight enough to be
// interesting would fail legitimate sandboxes as vCPU counts grow, and the failure
// would look like a boot that hangs.
//
// NOFILE is 1024 rather than jailer's 2048 only because jailer's number is itself
// arbitrary; both are far above what the VMM uses. NPROC is a process-wide count
// per uid, and because every sandbox on the node shares one uid it is *not* a
// per-sandbox bound -- pids.max in the cgroup is what bounds one sandbox. This
// bounds the uid as a whole, which is the fork-bomb case.
const (
	defaultVMMNoFile = 1024
	defaultVMMNProc  = 8192
)

// parseVMMCreds builds the credentials from an operator's uid and gid.
//
// A uid of 0 means the feature is off and returns nil, which is what makes an
// unconfigured node take the untouched path. Root is not a value worth supporting
// here: it is what the code already does, and accepting it would mean a
// misconfiguration that reads as "hardening on" while changing nothing.
//
// kvmGroup is the group that owns /dev/kvm, or 0 if it could not be read.
func parseVMMCreds(uid, gid int, kvmGroup int) (*vmmCreds, error) {
	if uid == 0 && gid == 0 {
		return nil, nil
	}
	if uid <= 0 {
		return nil, fmt.Errorf("vmm uid %d: must be a non-root uid, or 0 to run the "+
			"VMM as noded does", uid)
	}
	if gid <= 0 {
		return nil, fmt.Errorf("vmm gid %d: must be a non-root gid; it is set "+
			"separately from the uid because the sandbox directory is chowned to "+
			"both", gid)
	}
	c := &vmmCreds{
		UID:    uint32(uid),
		GID:    uint32(gid),
		NoFile: defaultVMMNoFile,
		NProc:  defaultVMMNProc,
	}
	// The primary gid is always in the supplementary set: with Credential set, Go
	// calls setgroups with exactly this list, so a gid left out of it is not one
	// the process has.
	c.Groups = append(c.Groups, c.GID)
	if kvmGroup > 0 && uint32(kvmGroup) != c.GID {
		c.Groups = append(c.Groups, uint32(kvmGroup))
	}
	return c, nil
}

// Enabled reports whether the VMM's identity is being changed.
func (c *vmmCreds) Enabled() bool { return c != nil }

// Summary is the startup line. It names the uid and the groups because the group
// is what carries /dev/kvm access, and a missing kvm group is the difference
// between a node that boots guests and one where every create fails on the same
// permission error.
func (c *vmmCreds) Summary() string {
	if c == nil {
		return "VMM runs with noded's own credentials (no privilege drop)"
	}
	groups := make([]string, 0, len(c.Groups))
	for _, g := range c.Groups {
		groups = append(groups, fmt.Sprint(g))
	}
	return fmt.Sprintf("VMM drops to uid %d gid %d (groups %s), "+
		"rlimits nofile=%d nproc=%d; the host filesystem stays visible to it, "+
		"confining that needs the mount namespace in GitHub #20 phase 2",
		c.UID, c.GID, strings.Join(groups, ","), c.NoFile, c.NProc)
}

// chown gives the dropped uid ownership of one path.
//
// Used for the things that are this sandbox's alone: its state directory, its
// device-mapper node and its UFFD socket. Shared assets are deliberately not
// chowned -- see checkSharedAssets.
func (c *vmmCreds) chown(path string) error {
	if c == nil {
		return nil
	}
	if err := os.Chown(path, int(c.UID), int(c.GID)); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, c.UID, c.GID, err)
	}
	return nil
}

// chownTree gives the dropped uid the sandbox directory and everything already in
// it.
//
// The directory has to be owned rather than merely readable: Firecracker creates
// its API socket and the vsock UDS inside it, and a snapshot's relative paths are
// resolved from it, so the process needs to both traverse and write it.
//
// The walk follows no symlinks. Two of the names in a sandbox directory are
// symlinks to targets outside it -- agent.ext4 to the shared agent disk and
// rootfs.img to /dev/mapper -- and a chown that followed them would take
// ownership of an asset shared with every other sandbox, or of a device node,
// from a function whose stated job is one sandbox's own files. os.Lchown on the
// link itself is a no-op for access (the kernel checks the target's mode) and is
// what keeps the walk honest.
func (c *vmmCreds) chownTree(dir string) error {
	if c == nil {
		return nil
	}
	var errs []error
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Collected rather than returned, so one unchownable file does not abandon
		// the rest of the directory: a VMM missing one of these fails at boot, and
		// the report naming every one of them is more useful than the first.
		if e := os.Lchown(path, int(c.UID), int(c.GID)); e != nil {
			errs = append(errs, fmt.Errorf("chown %s: %w", path, e))
		}
		return nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("walk %s: %w", dir, err))
	}
	return errors.Join(errs...)
}

// ensureTraversable gives every directory from base down to path the execute bit
// for others.
//
// This is for the snapshot cache. Its entries live under BaseDir/.snapshots at
// 0700 because noded is the only thing that has ever read them, and Firecracker
// opens the machine state file by absolute path during a restore. A dropped uid
// that cannot traverse those directories fails the load with a permission error
// naming a file it can see, which is at least an error -- unlike the memory image,
// which is never opened by Firecracker at all (noded mmaps it and serves faults
// over the UFFD socket) and so needs nothing here.
//
// Only the execute bit is added, and only on directories. Read is not: the dropped
// uid has no reason to list the cache, and o+x without o+r allows exactly "open a
// path I was told" while keeping the directory unlistable.
func (c *vmmCreds) ensureTraversable(base, path string) error {
	if c == nil {
		return nil
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("cannot relate %s to %s: %w", path, base, err)
	}
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not under %s", path, base)
	}
	dir := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		st, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if st.IsDir() && st.Mode().Perm()&0o001 == 0 {
			if err := os.Chmod(dir, st.Mode().Perm()|0o001); err != nil {
				return fmt.Errorf("chmod +x %s: %w", dir, err)
			}
		}
		if part == "." || part == "" {
			break
		}
		dir = filepath.Join(dir, part)
	}
	// The state file itself has to be readable. It is shared between every restore
	// of the snapshot, so it is made readable rather than chowned: chowning would
	// hand a shared cache entry to one sandbox's identity.
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() && st.Mode().Perm()&0o004 == 0 {
		if err := os.Chmod(path, st.Mode().Perm()|0o004); err != nil {
			return fmt.Errorf("chmod +r %s: %w", path, err)
		}
	}
	return nil
}

// checkSharedAssets reports the node-wide read-only assets the dropped uid cannot
// open.
//
// These are not chowned. The kernel image and the agent disk serve every sandbox
// on the node and are also read by noded itself, so making them the property of
// the sandbox uid would be moving a node asset into a sandbox's identity for no
// gain -- world-readable is what a shared read-only file wants to be.
//
// The check is by mode rather than by opening as the target uid, because noded is
// root and an open would succeed regardless of the mode, which is exactly the
// false pass that matters. Called at startup so the answer arrives as one message
// an operator can fix with a chmod, rather than as every create failing: a guest
// whose kernel cannot be opened does not boot, and a guest whose agent disk cannot
// be opened boots with no init.
func checkSharedAssets(paths ...string) []string {
	var bad []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if st.Mode().Perm()&0o004 == 0 {
			bad = append(bad, fmt.Sprintf("%s is mode %#o, not readable by others",
				p, st.Mode().Perm()))
		}
	}
	return bad
}
