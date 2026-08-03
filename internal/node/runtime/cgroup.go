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

// A cgroup around each sandbox's VMM process. cgroup v2 only.
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
// Why v2 is a node requirement rather than one of two supported hierarchies:
// **v1 cannot cap swap.** Its only swap-aware ceiling is
// memory.memsw.limit_in_bytes, which exists only when the kernel booted with
// swapaccount=1, and that is off by default on every distro kernel checked. On v1
// a VMM that reaches its ceiling is therefore pushed into swap rather than
// stopped, so the host thrashes while every log line says the limit is in force.
// Overcommitting memory for untrusted evaluation workloads is the entire purpose
// of this file, and swap thrashing is the precise failure the ceiling exists to
// prevent -- so on v1 "limits are in place" would be untrue in the dimension that
// matters most, which is worse than not supporting v1 at all. v2 spells it
// memory.swap.max and needs no boot parameter. bean picks its nodes; it does not
// have to accommodate whichever hierarchy a host happens to present.
//
// The ask is not exotic: systemd has defaulted to the unified hierarchy since
// v243, so Ubuntu 22.04+, Debian 11+, RHEL 9+ and anything newer are already v2.
// A v1 host is refused at startup rather than quietly downgraded to no limits --
// see detectCgroupHost in cgroup_linux.go for why the refusal is the point.
//
// The whole mechanism is off unless an operator asks for it. A node that has not
// configured it does not probe, does not log and does not create anything, so it
// behaves exactly as it did before this existed. That is deliberate: the memory
// ceiling below is derived from the guest's declared RAM plus a headroom that has
// not been measured against real workloads, and getting it too low does not
// degrade gracefully -- the kernel kills the VMM, which from the outside looks
// like a sandbox that died for no reason.

// The controllers this uses. Each is optional: a v2 host only exposes a
// controller's files in a child group if the parent delegated it through
// cgroup.subtree_control, and a kernel can be built without any one of them. So
// the usable set is probed rather than assumed, and which limits are actually in
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

// cgroupHost is the unified tree this node writes into, and the controllers it
// can actually use.
//
// A nil *cgroupHost means no limits, and every method tolerates it. That is the
// shape the "not configured" path has, and making it a nil receiver rather than a
// bool field means a caller that forgets to check gets no limits rather than a
// panic. Note what a nil host is *not*: a v1 host does not produce one, because a
// v1 host does not start. Detection refuses instead -- see detectCgroupHost.
type cgroupHost struct {
	// root is the mount point of the unified hierarchy, /sys/fs/cgroup.
	root string
	// controllers is the subset of cgroupControllers usable here, in
	// cgroupControllers order.
	controllers []string
}

// newCgroupHost describes what limits can be applied under root, which must be a
// cgroup v2 unified hierarchy.
//
// It never fails. A host with no usable controller yields a host with an empty
// controller set, which creates nothing and limits nothing -- because refusing to
// start a node over a controller the kernel was not built with would take a
// working node out of service to enforce a limit it was running fine without.
// That is a different case from a v1 host: v1 offers a ceiling that looks
// enforced and does not bound swap, whereas a missing controller is an absence
// the startup line names outright. The cost of this choice is that "limits
// requested" and "limits in force" can differ, so Summary exists to be logged
// once at startup and the two are never conflated.
func newCgroupHost(root string) *cgroupHost {
	if root == "" {
		return nil
	}
	h := &cgroupHost{root: root}
	// Probed once rather than per controller: one unified tree means one answer, and
	// the probe creates and removes a directory to find it.
	if !usable(root) {
		return h
	}
	for _, c := range cgroupControllers {
		// A directory being writable is not enough: a child group only has a
		// controller's files if the parent delegated the controller through
		// cgroup.subtree_control. Without this the group is created, memory.max does
		// not exist in it, and the limit is silently not applied -- the same shape of
		// bug as writing v1's filenames on a v2 host.
		if !enableV2Controller(root, c) {
			continue
		}
		h.controllers = append(h.controllers, c)
	}
	return h
}

// enableV2Controller makes a controller's files appear in root's child groups.
//
// Reported as a bool because the caller's only decision is whether the controller
// is usable, and the reasons it might not be -- not compiled in, not delegated to
// this cgroup namespace, no permission -- all lead to the same place.
//
// It follows the interface in Documentation/admin-guide/cgroup-v2.rst.
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

// usable reports whether the tree exists and this process may create a group in
// it. Both halves matter and neither is inferable from the other: an unprivileged
// noded, or one in a container whose cgroup namespace is read-only, sees the
// directory and cannot write it.
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

// baseDir is where per-sandbox groups live. One unified tree holds every
// controller, so this is the root itself: a sandbox gets exactly one directory
// however many controllers are in force.
func (h *cgroupHost) baseDir() string {
	if h == nil {
		return ""
	}
	return h.root
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
	s := fmt.Sprintf("cgroup v2 at %s, enforcing: %s", h.root,
		strings.Join(h.controllers, ","))
	if len(h.controllers) == 0 {
		s = fmt.Sprintf("cgroup v2 at %s, enforcing nothing", h.root)
	}
	if len(missing) > 0 {
		s += "; unavailable: " + strings.Join(missing, ",")
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

// writesFor renders one controller's limits into v2's interface files. Returning
// nothing means the limit is not expressible here, which is not the same as it
// being zero.
func (h *cgroupHost) writesFor(controller string, l cgroupLimits) []cgroupWrite {
	if h == nil {
		return nil
	}
	switch controller {
	case cgroupMemory:
		if l.MemoryBytes <= 0 {
			return nil
		}
		return []cgroupWrite{
			{file: "memory.max", value: strconv.FormatInt(l.MemoryBytes, 10)},
			// Swap is refused outright rather than bounded, and the value is fixed at
			// 0 rather than made configurable.
			//
			// This is the limit v2 is required for. memory.max alone does not stop a
			// VMM at its ceiling: the kernel's cheapest way to satisfy the next
			// allocation is to push pages to swap, so the group stays under its
			// ceiling while the host thrashes -- the limit reports success and the
			// node degrades anyway. Capping swap is what converts "stays under the
			// ceiling somehow" into "is stopped at the ceiling".
			//
			// Not configurable, because no value above 0 has a defensible meaning
			// here. Guest RAM is anonymous memory the guest kernel believes is real
			// RAM: it schedules against it, and it cannot see or wait on host swap.
			// A guest whose pages are on host swap is not a slower sandbox, it is one
			// the guest cannot distinguish from a hang, and evaluation workloads time
			// out rather than tolerate it. An operator wanting more room for a
			// sandbox should raise its memory, which raises memory.max through
			// limitsFor -- a knob whose effect is visible in the sandbox's spec,
			// rather than one that silently trades a kill for a stall.
			//
			// Optional so a kernel built without swap accounting (CONFIG_MEMCG_SWAP
			// off, where the file is absent) does not fail every create. That
			// absence is benign in a way v1's is not: no swap accounting means no
			// swap charged to the group at all, so the ceiling already bounds what
			// this write was protecting.
			{file: "memory.swap.max", value: "0", optional: true},
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
		// v2 writes the quota and the period as one pair, so there is no ordering
		// hazard: the kernel validates them against each other in a single write.
		return []cgroupWrite{{
			file:  "cpu.max",
			value: fmt.Sprintf("%d %d", quota, cgroupCPUPeriodUS),
		}}
	case cgroupPids:
		if l.PidsMax <= 0 {
			return nil
		}
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

// sandboxCgroup is one sandbox's group: a single directory in the unified tree,
// holding whichever controllers' files were delegated to it.
type sandboxCgroup struct {
	// dir is the directory to remove on teardown. Empty means nothing was created.
	dir string
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

	dir := filepath.Join(h.baseDir(), name)
	// MkdirAll rather than Mkdir: a group left behind by a previous noded that
	// died holds no processes (or the sweep would have skipped it), and refusing to
	// reuse it would make a create fail on a leftover directory instead of on
	// anything real.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup: create %s: %w", dir, err)
	}
	g := &sandboxCgroup{dir: dir}

	for _, c := range h.controllers {
		for _, w := range h.writesFor(c, l) {
			path := filepath.Join(dir, w.file)
			if err := os.WriteFile(path, []byte(w.value), 0o644); err != nil {
				if w.optional && errors.Is(err, os.ErrNotExist) {
					continue
				}
				// The group is removed here rather than left to the caller. A
				// half-built group is the leak class of GitHub #16: nothing afterwards
				// knows the directory exists, so nothing ever removes it.
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
	if g == nil || g.dir == "" {
		return nil
	}
	// One write covers every controller: in the unified hierarchy a process belongs
	// to one group, and every delegated controller charges it there.
	path := filepath.Join(g.dir, "cgroup.procs")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("cgroup: add pid %d to %s: %w", pid, path, err)
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
	if g == nil || g.dir == "" {
		return nil
	}
	dir := g.dir
	// Cleared before the result is known: a second Remove must be a no-op whether
	// or not the first succeeded, because the failed-create path and Destroy can
	// both reach it and a second error would mask the first.
	g.dir = ""
	if err := rmdirGroup(dir); err != nil {
		return fmt.Errorf("cgroup: remove %s: %w", dir, err)
	}
	return nil
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
	// One unified tree, so each sandbox appears exactly once and the counts are
	// sandboxes as well as directories.
	base := h.baseDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, 0
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
			// EBUSY: a sandbox that survived the restart is still in this group. Left
			// alone with its limits intact, and counted rather than disturbed.
			inUse++
			continue
		}
		removed++
	}
	return removed, inUse
}
