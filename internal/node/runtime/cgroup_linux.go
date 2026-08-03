//go:build linux

package runtime

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cgroupMountPoint is where both interface versions are mounted on every
// distribution checked. It is a constant rather than a parse of /proc/mounts
// because a host that has moved it has almost certainly also moved everything
// else this node assumes, and a wrong guess here fails closed: detection reports
// "unsupported" and the node runs with no limits, which is the pre-existing
// behaviour.
const cgroupMountPoint = "/sys/fs/cgroup"

// detectCgroupHost works out which cgroup interface this host presents.
//
// The version is detected rather than assumed because the two share a mount point
// and nothing else. Measured on the target host: /sys/fs/cgroup is tmpfs with one
// directory per controller (blkio, cpu, cpuacct, cpuset, devices, memory, pids,
// ...) and no cgroup.controllers file at all -- that is v1. A v2 host has
// cgroup2 as the filesystem type at the same path and one unified tree. Writing
// v2's memory.max on that v1 host would create nothing and enforce nothing, and
// the only evidence would be an ENOENT nobody reads.
//
// A nil return means no limits will be applied, and that is a supported state:
// see newCgroupHost for why a node in it still starts.
func detectCgroupHost() *cgroupHost {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupMountPoint, &st); err != nil {
		return nil
	}
	switch st.Type {
	case unix.CGROUP2_SUPER_MAGIC:
		return newCgroupHost(cgroupMountPoint, cgroupV2)
	case unix.TMPFS_MAGIC:
		// tmpfs at the mount point is v1's layout, and also the hybrid layout
		// where v1 controllers sit beside a controllerless cgroup2 tree at
		// /sys/fs/cgroup/unified. Both are answered the same way: the controllers
		// that carry limits are the v1 ones, so this is v1. The hybrid tree is
		// ignored rather than preferred, because in hybrid mode it has no
		// controllers to enable -- a group there would limit nothing.
		if !anyV1Controller(cgroupMountPoint) {
			return nil
		}
		return newCgroupHost(cgroupMountPoint, cgroupV1)
	}
	return nil
}

// anyV1Controller reports whether at least one of the controllers this package
// uses is mounted as a v1 hierarchy.
//
// The filesystem type is checked rather than the directory's existence: a plain
// directory under the tmpfs looks identical to a mounted controller from a stat,
// and writing limits into one silently does nothing.
func anyV1Controller(root string) bool {
	for _, c := range cgroupControllers {
		var st unix.Statfs_t
		if err := unix.Statfs(filepath.Join(root, c), &st); err != nil {
			continue
		}
		if st.Type == unix.CGROUP_SUPER_MAGIC {
			return true
		}
	}
	return false
}

// The v2 subtree_control handling lives in cgroup.go with the rest of the
// version-independent file writing: it is ordinary reads and writes rather than
// syscalls, and keeping it there is what lets a test build a fake v2 tree and
// assert on it without a Linux kernel.
