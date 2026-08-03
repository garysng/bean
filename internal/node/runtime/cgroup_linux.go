//go:build linux

package runtime

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// cgroupMountPoint is where the unified hierarchy is mounted on every
// distribution checked. It is a constant rather than a parse of /proc/mounts
// because a host that has moved it has almost certainly also moved everything
// else this node assumes.
const cgroupMountPoint = "/sys/fs/cgroup"

// detectCgroupHost requires a cgroup v2 unified hierarchy, and refuses anything
// else.
//
// The filesystem type is what distinguishes the two: v2 is cgroup2 at this path,
// while v1 (and the hybrid layout) is a tmpfs with one directory per controller
// under it. Statfs is asked rather than the directory listing guessed at, because
// a plain directory under a tmpfs is indistinguishable from a mounted controller
// by stat alone.
//
// Two failures are deliberately kept distinct here, and the distinction is the
// whole point of this function:
//
//   - Not configured at all (--fc-cgroups off) never reaches this code. That node
//     runs with no limits, says so, and is honest: an operator chose it, and it is
//     what a development machine or a CI container without permission needs.
//   - A v1 host reaches this code and gets an error, which cmd/noded turns into a
//     refusal to start. It does *not* fall back to running without limits. A
//     silent downgrade would leave somebody believing there is a boundary where
//     there is none, which is the failure mode docs/security-and-startup.md A3
//     records as the most expensive kind of error in this area -- and worse in
//     code than in prose, because an operator raises --overcommit-memory on the
//     strength of it. v1's specific defect is that it cannot cap swap
//     (memory.memsw.limit_in_bytes needs swapaccount=1, off by default), so its
//     ceiling lets a guest drive the host into swap thrashing while every log line
//     reports the limit in force. That is the exact failure the limit exists to
//     prevent, in the exact scenario -- overcommitted memory for untrusted
//     evaluation workloads -- this feature was built for.
func detectCgroupHost() (*cgroupHost, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupMountPoint, &st); err != nil {
		return nil, fmt.Errorf("cgroup: %s is not readable, so no limit can be "+
			"applied: %w; run without --fc-cgroups to start this node with no "+
			"kernel-enforced limit, and do not raise --overcommit-memory on it",
			cgroupMountPoint, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		// The remedy is named because it is a boot-time change an operator cannot
		// guess from "wrong cgroup version", and because the alternative -- start
		// anyway -- is the thing this refusal exists to rule out.
		return nil, fmt.Errorf("cgroup: %s is not a cgroup v2 unified hierarchy "+
			"(filesystem type 0x%x, expected cgroup2 0x%x); bean requires v2 because "+
			"v1 cannot cap swap: memory.memsw.limit_in_bytes needs the kernel booted "+
			"with swapaccount=1, so a v1 memory ceiling lets a guest push the host "+
			"into swap thrashing while reporting the limit as enforced, which is the "+
			"failure raising --overcommit-memory depends on this limit to prevent. "+
			"Boot with systemd.unified_cgroup_hierarchy=1 (Ubuntu 22.04+, Debian 11+ "+
			"and RHEL 9+ are already v2), or run without --fc-cgroups to start with "+
			"no limits at all",
			cgroupMountPoint, uint64(st.Type), uint64(unix.CGROUP2_SUPER_MAGIC))
	}
	return newCgroupHost(cgroupMountPoint), nil
}

// The subtree_control handling lives in cgroup.go with the rest of the file
// writing: it is ordinary reads and writes rather than syscalls, and keeping it
// there is what lets a test build a fake v2 tree and assert on it without a Linux
// kernel.
