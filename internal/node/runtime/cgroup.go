package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A cgroup around each sandbox's VMM process.
//
// Firecracker enforces the guest's own memory configuration, so a guest cannot
// exceed the RAM it was given. What has no ceiling today is the VMM process on
// the host: its page tables, its device model, the memory image it faults in and
// anything it leaks are charged to the host with nothing bounding them. That is
// why internal/node/overcommit.go and cmd/noded/main.go refuse to raise memory
// overcommit above 1.0 -- the scheduler's ledger is the only limit in play, and a
// ledger does not stop a process. This file is what turns the ledger into
// something the kernel enforces.
//
// The whole mechanism is off unless an operator asks for it. A node that has not
// configured it does not probe, does not log and does not create anything, so it
// behaves exactly as it did before this existed. That is deliberate: the memory
// ceiling below is derived from the guest's declared RAM plus a headroom that has
// not been measured against real workloads, and getting it too low does not
// degrade gracefully -- the kernel kills the VMM, which from the outside looks
// like a sandbox that died for no reason.

// cgroupVersion is which cgroup interface a host presents. The two are not
// variations on a spelling: v1 mounts one tree per controller and writes
// memory.limit_in_bytes and cpu.cfs_quota_us, v2 mounts a single unified tree and
// writes memory.max and cpu.max. Code that assumes either one is silently
// ineffective on the other, because a write to a file that does not exist is the
// only symptom and nothing reads it back.
type cgroupVersion int

const (
	// cgroupUnsupported is a host with neither interface mounted where this
	// process can reach it. It is a supported outcome, not an error: see
	// newCgroupHost.
	cgroupUnsupported cgroupVersion = iota
	cgroupV1
	cgroupV2
)

func (v cgroupVersion) String() string {
	switch v {
	case cgroupV1:
		return "v1"
	case cgroupV2:
		return "v2"
	default:
		return "unsupported"
	}
}

// The controllers this uses. Each is optional: a host can mount v1's memory tree
// and not its pids tree, and a v2 host only exposes a controller's files in a
// child group if the parent delegated it through cgroup.subtree_control. So the
// usable set is probed rather than assumed, and which limits are actually in
// force is reported at startup -- an operator who believes a limit is enforced
// when it is not is the failure mode the A3 documentation error had.
const (
	cgroupMemory = "memory"
	cgroupCPU    = "cpu"
	cgroupPids   = "pids"
)

// cgroupControllers is the order controllers are created and written in. Fixed so
// the startup log and the tests do not depend on map iteration.
var cgroupControllers = []string{cgroupMemory, cgroupCPU, cgroupPids}

// cgroupPrefix namespaces bean's groups inside a tree it shares with systemd,
// Docker and anything else on the host. Every name this package creates carries
// it, and the startup sweep will only remove a directory that has it, so a bug
// here cannot reach another workload's cgroup.
const cgroupPrefix = "bean-"

// cgroupCPUPeriodUS is the scheduling window a CPU quota is expressed against.
// 100ms is the kernel's own default and what every other cgroup user on the host
// will be using; a shorter window bounds latency more tightly at the cost of more
// scheduler work, and there is no reason for bean to differ.
const cgroupCPUPeriodUS = 100000

// vmmMemoryHeadroomMiB is how much the VMM's memory ceiling exceeds the RAM its
// guest declares.
//
// It cannot be zero. Guest RAM is anonymous memory in the VMM's own address
// space, so a ceiling equal to the guest's configuration is one the guest reaches
// simply by touching all of its pages -- and the kernel's answer is to kill the
// VMM, which presents as a sandbox that died with nothing in its logs. The
// headroom covers the VMM's own footprint: the device model, one stack per vCPU
// thread, the binary, and the page tables for the guest's memory.
//
// The value is not measured against real workloads. It is deliberately generous
// for that reason: too high only weakens the limit, while too low destroys
// sandboxes.
const vmmMemoryHeadroomMiB = 256

// cgroupHost is the tree this node writes into, and the controllers it can
// actually use.
//
// A nil *cgroupHost means no limits, and every method tolerates it. That is the
// shape the "not configured" and "host has neither interface" paths share, and
// making it a nil receiver rather than a bool field means a caller that forgets
// to check gets no limits rather than a panic.
type cgroupHost struct {
	// root is the mount point: /sys/fs/cgroup on both versions, though what lives
	// under it differs.
	root string
	// version decides which files hold the limits.
	version cgroupVersion
	// controllers is the subset of cgroupControllers usable here, in
	// cgroupControllers order.
	controllers []string
}

// newCgroupHost describes what limits can be applied under root at version.
//
// It never fails. A host with no usable controller yields a host with an empty
// controller set, which creates nothing and limits nothing -- because refusing to
// start a node over a missing cgroup controller would take a working node out of
// service to enforce a limit it was running fine without. The cost of that choice
// is that "limits requested" and "limits in force" can differ, so Summary exists
// to be logged once at startup and the two are never conflated.
func newCgroupHost(root string, version cgroupVersion) *cgroupHost {
	if root == "" || version == cgroupUnsupported {
		return nil
	}
	h := &cgroupHost{root: root, version: version}
	for _, c := range cgroupControllers {
		if !usable(h.baseDir(c)) {
			continue
		}
		// On v2 a directory being writable is not enough: a child group only has a
		// controller's files if the parent delegated the controller through
		// cgroup.subtree_control. Without this the group is created, memory.max
		// does not exist in it, and the limit is silently not applied.
		if version == cgroupV2 && !enableV2Controller(root, c) {
			continue
		}
		h.controllers = append(h.controllers, c)
	}
	return h
}

// enableV2Controller makes a controller's files appear in root's child groups.
//
// v2-only, and it has no v1 analogue: on v1 a controller's files exist in every
// group of its own hierarchy, so there is nothing to enable. Reported as a bool
// because the caller's only decision is whether the controller is usable, and the
// reasons it might not be -- not compiled in, not delegated to this cgroup
// namespace, no permission -- all lead to the same place.
//
// Unexercised against a kernel: the target host is v1. It follows the interface in
// Documentation/admin-guide/cgroup-v2.rst.
func enableV2Controller(root, controller string) bool {
	avail, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return false
	}
	if !fieldPresent(string(avail), controller) {
		return false
	}
	enabled, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err == nil && fieldPresent(string(enabled), controller) {
		return true
	}
	// The "+name" form enables one controller and leaves the rest alone. Writing
	// the whole set would disable anything this node did not ask for, on a tree it
	// shares with systemd.
	return os.WriteFile(filepath.Join(root, "cgroup.subtree_control"),
		[]byte("+"+controller), 0o644) == nil
}

// fieldPresent reports whether a space-separated controller list contains name.
// Substring matching would be wrong here: "cpu" is a prefix of "cpuset", and
// treating the latter as the former would enable a controller that has none of
// the files the limits need.
func fieldPresent(list, name string) bool {
	for _, f := range strings.Fields(list) {
		if f == name {
			return true
		}
	}
	return false
}

// usable reports whether a controller's tree exists and this process may create a
// group in it. Both halves matter and neither is inferable from the other: an
// unprivileged noded, or one in a container whose cgroup namespace is read-only,
// sees the directory and cannot write it.
func usable(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	// Probed by creating and removing a directory rather than by checking mode
	// bits, because the answer depends on the mount's read-only flag and on
	// capabilities, neither of which the mode shows.
	probe := filepath.Join(dir, cgroupPrefix+"probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// baseDir is where per-sandbox groups for a controller live. On v2 every
// controller shares one tree, which is the whole difference between the versions
// as far as paths go.
func (h *cgroupHost) baseDir(controller string) string {
	if h == nil {
		return ""
	}
	if h.version == cgroupV2 {
		return h.root
	}
	return filepath.Join(h.root, controller)
}

// Enabled reports whether any limit will actually be applied.
func (h *cgroupHost) Enabled() bool { return h != nil && len(h.controllers) > 0 }

// Summary is the one line an operator needs at startup: which interface was
// found, and which limits are in force. It names the limits that are *not* in
// force too, because a partial answer read as a complete one is how somebody
// raises memory overcommit on a node with no memory controller.
func (h *cgroupHost) Summary() string {
	if h == nil {
		return "no cgroup interface found; the VMM runs unlimited on the host"
	}
	missing := []string{}
	for _, c := range cgroupControllers {
		if !h.has(c) {
			missing = append(missing, c)
		}
	}
	s := fmt.Sprintf("cgroup %s at %s, enforcing: %s", h.version, h.root,
		strings.Join(h.controllers, ","))
	if len(h.controllers) == 0 {
		s = fmt.Sprintf("cgroup %s at %s, enforcing nothing", h.version, h.root)
	}
	if len(missing) > 0 {
		s += "; unavailable: " + strings.Join(missing, ",")
	}
	if h.version == cgroupV1 {
		// Stated rather than left as an absence. v2's memory.swap.max=0 has no
		// equivalent on v1 unless the kernel booted with swapaccount=1, so on a v1
		// host a VMM at its memory ceiling can be pushed into swap instead of
		// being stopped. The limit still bounds RAM; it does not bound swap.
		s += "; v1 cannot cap swap (memory.swap.max is v2-only)"
	}
	return s
}

func (h *cgroupHost) has(controller string) bool {
	if h == nil {
		return false
	}
	for _, c := range h.controllers {
		if c == controller {
			return true
		}
	}
	return false
}

// cgroupLimits is what one sandbox's VMM is allowed. A zero field means that
// limit is not applied, which is what a spec that does not state a size gets:
// inventing a ceiling for a sandbox nobody sized would kill it on a number
// nothing in the request explains.
type cgroupLimits struct {
	// MemoryBytes caps the VMM's memory, guest RAM included.
	MemoryBytes int64
	// CPUCores caps CPU time as a fraction of one core per core requested.
	CPUCores float64
	// PidsMax caps processes and threads in the group.
	PidsMax int64
}

// cgroupPidsMax bounds threads and processes in one VMM's group.
//
// Firecracker runs one thread per vCPU plus a handful of its own, so this is
// orders of magnitude above anything a healthy VMM needs. It is not sized to be
// tight; it is sized to stop a VMM that is forking without bound from taking the
// node's pid space with it, which is cheap insurance and the reason the pids
// controller is worth using at all.
const cgroupPidsMax = 512

// limitsFor derives one sandbox's limits from its own spec.
//
// Memory comes from the guest's declared RAM plus vmmMemoryHeadroomMiB, and CPU
// from the same vCPU count the machine configuration gets, so the ceiling and the
// guest are two views of one number rather than two numbers that can drift.
func limitsFor(spec *Spec) cgroupLimits {
	var l cgroupLimits
	l.PidsMax = cgroupPidsMax
	if spec == nil {
		return l
	}
	if spec.MemoryMiB > 0 {
		l.MemoryBytes = (spec.MemoryMiB + vmmMemoryHeadroomMiB) << 20
	}
	if spec.CPU > 0 {
		l.CPUCores = spec.CPU
	}
	return l
}

// cgroupWrite is one limit file and the value to put in it.
type cgroupWrite struct {
	file  string
	value string
	// optional marks a file whose absence is a property of the host rather than a
	// failure. Only used where the absence is understood and stated.
	optional bool
}

// writesFor renders one controller's limits into the files this host's interface
// version actually has. Returning nothing means the limit is not expressible
// here, which is not the same as it being zero.
func (h *cgroupHost) writesFor(controller string, l cgroupLimits) []cgroupWrite {
	if h == nil {
		return nil
	}
	switch controller {
	case cgroupMemory:
		if l.MemoryBytes <= 0 {
			return nil
		}
		if h.version == cgroupV2 {
			return []cgroupWrite{
				{file: "memory.max", value: strconv.FormatInt(l.MemoryBytes, 10)},
				// Swap is refused outright rather than bounded: a VMM whose guest
				// pages reach swap is one whose sandbox has become unusably slow,
				// and the guest cannot tell that from a hang.
				{file: "memory.swap.max", value: "0", optional: true},
			}
		}
		// v1. memory.memsw.limit_in_bytes is the nearest thing to swap.max and it
		// only exists when the kernel booted with swapaccount=1, which is off by
		// default on every distro kernel checked. Optional for that reason, and
		// its absence is reported in Summary rather than passed over.
		return []cgroupWrite{
			{file: "memory.limit_in_bytes", value: strconv.FormatInt(l.MemoryBytes, 10)},
			{file: "memory.memsw.limit_in_bytes", value: strconv.FormatInt(l.MemoryBytes, 10), optional: true},
		}
	case cgroupCPU:
		if l.CPUCores <= 0 {
			return nil
		}
		quota := int64(l.CPUCores * cgroupCPUPeriodUS)
		if quota < 1000 {
			// The kernel rejects a quota below 1ms. A sandbox asking for a
			// thousandth of a core is not a case worth failing a create over, so
			// it is floored instead.
			quota = 1000
		}
		if h.version == cgroupV2 {
			return []cgroupWrite{{
				file:  "cpu.max",
				value: fmt.Sprintf("%d %d", quota, cgroupCPUPeriodUS),
			}}
		}
		// v1 splits the pair across two files, and the period must be written
		// first: the kernel validates a new quota against the period already in
		// place, so writing the quota first can be rejected as out of range.
		return []cgroupWrite{
			{file: "cpu.cfs_period_us", value: strconv.Itoa(cgroupCPUPeriodUS)},
			{file: "cpu.cfs_quota_us", value: strconv.FormatInt(quota, 10)},
		}
	case cgroupPids:
		if l.PidsMax <= 0 {
			return nil
		}
		// The one file both versions spell the same way.
		return []cgroupWrite{{file: "pids.max", value: strconv.FormatInt(l.PidsMax, 10)}}
	}
	return nil
}

// cgroupNameFor is the directory name for a sandbox's group.
//
// It refuses anything that is not a single path element. Sandbox ids arrive from
// the control plane and are used here to build a path that is removed later, so
// an id containing a separator would let a create reach outside the tree and a
// teardown remove something that was never bean's.
func cgroupNameFor(id string) (string, error) {
	if id == "" {
		return "", errors.New("cgroup: sandbox id required")
	}
	if strings.ContainsAny(id, "/\x00") || id == "." || id == ".." {
		return "", fmt.Errorf("cgroup: sandbox id %q is not a single path element", id)
	}
	return cgroupPrefix + id, nil
}

// sandboxIDFromCgroupName is the inverse, written as one so the two cannot
// disagree: the startup sweep decides what to remove from this answer, and the
// same reasoning as image.SandboxIDFromDMName applies -- a mismatch either leaks
// groups forever or removes a stranger's.
func sandboxIDFromCgroupName(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, cgroupPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// sandboxCgroup is one sandbox's group, across however many controller trees the
// host's interface version needs.
type sandboxCgroup struct {
	// dirs are the directories to remove on teardown, in creation order.
	dirs []string
}

// createCgroup builds the group and writes its limits, but does not put anything
// in it. The two are separate because the order is load-bearing: a process added
// before the limits are written runs unbounded for as long as that takes.
//
// A nil host, or one with no usable controller, returns a nil group. Every method
// on *sandboxCgroup tolerates nil, so the caller has one path.
func (h *cgroupHost) createCgroup(id string, l cgroupLimits) (*sandboxCgroup, error) {
	if !h.Enabled() {
		return nil, nil
	}
	name, err := cgroupNameFor(id)
	if err != nil {
		return nil, err
	}

	g := &sandboxCgroup{}
	for _, c := range h.controllers {
		dir := filepath.Join(h.baseDir(c), name)
		// MkdirAll rather than Mkdir: a group left behind by a previous noded that
		// died holds no processes (or the sweep would have skipped it), and
		// refusing to reuse it would make a create fail on a leftover directory
		// instead of on anything real.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// Everything created so far is removed here rather than left to the
			// caller. A half-built group is the leak class of GitHub #16: nothing
			// afterwards knows the directory exists, so nothing ever removes it.
			_ = g.Remove()
			return nil, fmt.Errorf("cgroup: create %s: %w", dir, err)
		}
		g.dirs = append(g.dirs, dir)

		for _, w := range h.writesFor(c, l) {
			path := filepath.Join(dir, w.file)
			if err := os.WriteFile(path, []byte(w.value), 0o644); err != nil {
				if w.optional && errors.Is(err, os.ErrNotExist) {
					continue
				}
				_ = g.Remove()
				return nil, fmt.Errorf("cgroup: write %s=%s: %w", path, w.value, err)
			}
		}
	}
	return g, nil
}

// Add puts a process in the group, which is the point at which the limits start
// applying to it.
func (g *sandboxCgroup) Add(pid int) error {
	if g == nil {
		return nil
	}
	for _, dir := range g.dirs {
		// cgroup.procs is the file on both versions. Writing a pid moves the whole
		// process; on v1 that is per-controller, which is why this loops.
		path := filepath.Join(dir, "cgroup.procs")
		if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return fmt.Errorf("cgroup: add pid %d to %s: %w", pid, path, err)
		}
	}
	return nil
}

// Remove tears the group down.
//
// rmdir is the only way to delete a cgroup and the kernel refuses it while the
// group holds a process, so this cannot destroy a live sandbox's limits by
// mistake -- and a failure here means a VMM is still in the group, which is worth
// reporting rather than retrying blindly.
func (g *sandboxCgroup) Remove() error {
	if g == nil {
		return nil
	}
	var errs []error
	// Reverse creation order for symmetry with the cleanup stacks elsewhere in
	// this package; the controllers are independent, so order is not otherwise
	// load-bearing.
	for i := len(g.dirs) - 1; i >= 0; i-- {
		if err := rmdirGroup(g.dirs[i]); err != nil {
			errs = append(errs, fmt.Errorf("cgroup: remove %s: %w", g.dirs[i], err))
		}
	}
	g.dirs = nil
	return errors.Join(errs...)
}

// rmdirGroup removes one cgroup directory.
//
// The plain rmdir is the whole operation on real cgroupfs: a group's interface
// files (memory.max, cgroup.procs and the rest) are not dirents that block it, so
// a group with no child groups and no processes is removable as it stands. Two
// distinguishable refusals matter and both are preserved here -- EBUSY means the
// group still holds a process, and ENOTEMPTY means it still has a child group.
//
// The retry exists because a directory holding ordinary files is not removable on
// an ordinary filesystem, and that is what a test's fake tree is. Without it every
// teardown assertion in cgroup_test.go would fail for a reason that has nothing to
// do with the code being tested, and the leak the tests exist to catch would be
// untestable off a Linux host. It is safe on the real thing: unlinking a cgroup
// interface file returns EPERM, so nothing is destroyed and the original refusal
// stands. It is also safe against a child group, because only regular files are
// unlinked and a surviving subdirectory keeps the rmdir refused.
func rmdirGroup(dir string) error {
	err := os.Remove(dir)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			// A child group. Not bean's to remove, and its presence is the reason
			// for the refusal.
			return err
		}
		if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr != nil {
			return err
		}
	}
	if second := os.Remove(dir); second != nil && !errors.Is(second, os.ErrNotExist) {
		return second
	}
	return nil
}

// SweepOrphans removes bean's groups that no longer hold a process.
//
// This is the same leak as GitHub #16's loop devices: Destroy removes the group,
// a noded that is killed never reaches Destroy, and the directory stays for the
// life of the host. It is swept here rather than in internal/node/reclaim
// deliberately -- see docs/security-and-startup.md A3. reclaim's decisions rest
// on inference from the control plane's expected-sandbox set, because a dm
// mapping cannot be asked whether anyone is using it. A cgroup can: rmdir fails
// with EBUSY while the group holds a process, so "is this in use" is answered by
// the kernel rather than guessed, and a sweep that only removes what rmdir
// accepts needs no expected set and cannot race a running sandbox.
//
// It returns the number removed and the number left alone. Errors are not
// returned: nothing here is worth failing a startup over, and a group that could
// not be removed is one that is still in use.
func (h *cgroupHost) SweepOrphans() (removed, inUse int) {
	if !h.Enabled() {
		return 0, 0
	}
	// A group exists in every controller tree, so the same sandbox is visited once
	// per controller. Counting directories rather than sandboxes keeps this honest
	// about what it did to the filesystem.
	for _, c := range h.controllers {
		base := h.baseDir(c)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := sandboxIDFromCgroupName(e.Name()); !ok {
				// Not bean's. Not counted, not touched, not reported.
				continue
			}
			if err := rmdirGroup(filepath.Join(base, e.Name())); err != nil {
				// EBUSY: a sandbox that survived the restart is still in this group.
				// Left alone with its limits intact, and counted rather than
				// disturbed.
				inUse++
				continue
			}
			removed++
		}
	}
	return removed, inUse
}
