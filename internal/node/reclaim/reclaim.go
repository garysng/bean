// Package reclaim returns host resources that a previous noded left behind.
//
// The device-mapper mapping, loop device and sandbox directory backing a sandbox
// are undone by Rootfs.release, which runs on the destroy path. A process that is
// killed never reaches it, so everything it held stays on the host: the mapping
// pins a loop device, the loop device pins a file, and the file holds disk the
// scheduler has already counted as free. GitHub #16 fixed the narrower case of
// re-attaching a base image on restart by adopting the existing device; the
// resources here have no owner left to adopt them, so they need removing.
//
// The reason this is written carefully rather than as a sweep is that the hosts
// this runs on are shared. Docker's thin pools, nexus pods and snapd all create
// device-mapper mappings and loop devices, and destroying one of theirs is not
// recoverable by restarting anything. Two rules follow, and every decision below
// is one of them applied:
//
//   - Only bean's own names and paths are visible here. A mapping without the
//     bean- prefix, or a loop device backed by a file outside the directories
//     this node owns, is not considered at all — not kept, not counted, not seen.
//   - Uncertainty means reporting, not removing. A leak that is logged and
//     counted costs disk until someone looks; a mapping removed from under a
//     running guest costs that guest's filesystem, silently and permanently
//     (see diskguard.go for what a broken dm-snapshot target does to a guest).
//
// Cgroups are a leftover of the same kind and are deliberately *not* handled
// here. GitHub #20 phase 1 puts each VMM in a per-sandbox cgroup, and a noded
// that is killed leaves the directory behind exactly as it leaves a mapping
// behind. They are swept by the runtime instead
// (runtime.cgroupHost.SweepOrphans), because the reason this package needs the
// control plane's expected-sandbox set does not apply to them: a dm mapping
// cannot be asked whether anyone is using it, so ownership has to be inferred,
// while rmdir on a cgroup fails with EBUSY for exactly as long as the group holds
// a process. That makes "is this in use" a question the kernel answers, and a
// sweep that removes only what rmdir accepts cannot race a running sandbox and
// needs nothing from the control plane. Extending the Host interface to cover them
// would add an expected set to a decision that does not need one.
package reclaim

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/obs"
)

// LoopDevice is one loop device as the host reports it.
type LoopDevice struct {
	// Name is the device path, e.g. /dev/loop15.
	Name string
	// BackingFile is the path the device was attached to. It is still reported
	// after the file is unlinked, which is what makes a dead device attributable
	// to the directory it came from.
	BackingFile string
	// Deleted marks a device whose backing file has been unlinked. Nothing can
	// reopen such a file, so the only thing the device still does is hold its
	// blocks against the filesystem.
	Deleted bool
}

// Host is the boundary between the decisions in this file and the commands that
// carry them out.
//
// It exists so those decisions can be tested. Reconciliation is the code most in
// need of tests and least able to have them if it shells out directly: exercising
// it for real needs root, a spare kernel and a willingness to have a bug delete
// the wrong mapping on the machine running the test.
type Host interface {
	// ListDMNames lists every device-mapper mapping on the host, bean's and
	// everyone else's. Filtering is this package's job so that the filter is
	// where the tests can reach it.
	ListDMNames() ([]string, error)
	// RemoveDM tears down one mapping. It must fail rather than force when the
	// device is open.
	RemoveDM(name string) error
	// ListLoopDevices lists every loop device with its backing file.
	ListLoopDevices() ([]LoopDevice, error)
	// DetachLoop releases one loop device. It must fail rather than force when
	// the device is in use.
	DetachLoop(dev string) error
	// ListSandboxDirs lists the directory names directly under the sandbox base
	// directory.
	ListSandboxDirs() ([]string, error)
	// RemoveSandboxDir removes one sandbox directory and its contents.
	RemoveSandboxDir(name string) error
}

// Reconciler compares host state against the sandboxes that are supposed to
// exist, and reclaims what is provably left over.
type Reconciler struct {
	// BaseDir holds one directory per sandbox, each with the sparse
	// copy-on-write store behind that sandbox's rootfs.
	BaseDir string
	// ImageDir holds the shared base images. Loop devices backed by files here
	// are read-only bases serving an unknown number of sandboxes, so they are
	// recognised in order to be left alone deliberately rather than by omission.
	ImageDir string
	// Host performs the inspection and the removals.
	Host Host
	// Metrics receives the counts. Nil is allowed so tests can assert on the
	// report alone.
	Metrics *obs.Registry
}

// Report is what one pass found and did. Every field is also a metric; the
// struct exists so a test can assert on the outcome without reading the
// registry, and so the summary log line is one statement rather than several.
type Report struct {
	// Found counts orphans identified, by resource kind.
	Found map[string]int
	// Reclaimed counts orphans successfully removed.
	Reclaimed map[string]int
	// Failed counts orphans that were identified but could not be removed.
	// These are still on the host.
	Failed map[string]int
	// Kept counts resources matching bean's prefix that were left alone because
	// they belong to a sandbox that is supposed to exist.
	Kept map[string]int
	// Suspect describes resources that look wrong but could not be shown to be
	// orphans, so they were left alone. This is the outcome that needs a human:
	// the situations that land here are the ones the rules above do not cover.
	Suspect []string
}

// Resource kinds, used as both map keys and metric label values.
const (
	kindMapping = "dm_mapping"
	kindLoop    = "loop_device"
	kindDir     = "sandbox_dir"
)

func newReport() Report {
	return Report{
		Found:     map[string]int{},
		Reclaimed: map[string]int{},
		Failed:    map[string]int{},
		Kept:      map[string]int{},
	}
}

// Run performs one reconciliation pass.
//
// expected is the sandbox set the control plane says this node should be
// running, from SyncState. It is required: without it every resource on the host
// looks like an orphan, and the whole point of consulting it is that a sandbox
// started before this process began is indistinguishable from garbage by looking
// at the host alone. Run refuses an empty-but-unknown set by taking it as a
// parameter rather than fetching it, so a caller cannot accidentally reconcile
// against a failed lookup.
//
// An error is returned only when the host could not be inspected at all. A
// failure to remove one resource is recorded in the report and does not stop the
// pass: the resources are independent, and stopping early would leave the rest
// unexamined for no gain.
func (r *Reconciler) Run(expected map[string]bool) (Report, error) {
	rep := newReport()
	if r.Host == nil {
		return rep, fmt.Errorf("reclaim: no host")
	}

	// Mappings first. A mapping holds its loop device open, and the loop device
	// holds the file in the sandbox directory, so unwinding in any other order
	// fails on a busy device and leaves everything in place.
	st := &state{}
	r.mappings(expected, st, &rep)
	r.loops(expected, st, &rep)
	r.dirs(expected, st, &rep)

	r.publish(rep)
	return rep, nil
}

// state carries what each stage learned to the ones that depend on it.
//
// The unknown flags are the load-bearing part. A stage that could not inspect the
// host has to be distinguishable from one that found nothing, because "no mapping
// holds this file" and "it is not known whether a mapping holds this file" lead to
// opposite decisions, and conflating them is how a live guest loses its rootfs.
type state struct {
	// mappingsUnknown is set when the mapping list could not be read.
	mappingsUnknown bool
	// mappingAlive names sandboxes whose mapping is still standing, either because
	// they are expected or because removal failed.
	mappingAlive map[string]bool

	// loopsUnknown is set when the loop device list could not be read.
	loopsUnknown bool
	// loopHeld names sandboxes with a loop device still attached to a file of
	// theirs after this pass.
	loopHeld map[string]bool
}

func (s *state) markMappingAlive(id string) {
	if s.mappingAlive == nil {
		s.mappingAlive = map[string]bool{}
	}
	s.mappingAlive[id] = true
}

func (s *state) markLoopHeld(id string) {
	if s.loopHeld == nil {
		s.loopHeld = map[string]bool{}
	}
	s.loopHeld[id] = true
}

// mappings removes the dm mappings whose sandbox the control plane does not
// expect, and records which sandboxes still have one.
//
// That record is what lets the later stages act: a loop device or directory
// belonging to a sandbox whose mapping is still standing must be left alone,
// because the mapping is still reading from it.
func (r *Reconciler) mappings(expected map[string]bool, st *state, rep *Report) {
	names, err := r.Host.ListDMNames()
	if err != nil {
		// Not fatal, but it bounds what the rest of the pass may do: without
		// knowing which mappings exist, no loop device or directory can be shown to
		// be unreferenced.
		st.mappingsUnknown = true
		rep.Suspect = append(rep.Suspect, fmt.Sprintf(
			"cannot list device-mapper mappings (%v); loop devices and sandbox "+
				"directories were left alone because nothing can be shown unreferenced "+
				"without it", err))
		return
	}

	for _, name := range names {
		id, ok := image.SandboxIDFromDMName(name)
		if !ok {
			// Someone else's mapping. Not counted, because a count invites
			// somebody to act on it.
			continue
		}
		if expected[id] {
			rep.Kept[kindMapping]++
			st.markMappingAlive(id)
			continue
		}
		rep.Found[kindMapping]++
		slog.Info("reclaiming orphaned device-mapper mapping",
			logging.KeySandbox, id, "mapping", name)
		if err := r.Host.RemoveDM(name); err != nil {
			// A busy device: something still has it open, most likely a firecracker
			// process that outlived the noded that started it. Forcing the removal
			// here would take the device out from under a guest that is still
			// writing to it, so the mapping stays and the operator gets told.
			//
			// This used to say the failure is "almost always" busy. It was not: on a
			// 300-sandbox burst, 109 of these were "No such device or address" --
			// the mapping had already been removed by the sandbox destroying itself
			// concurrently. RemoveDM now reports that case as success, because
			// marking an absent mapping alive blocks the loop device and directory
			// behind it and turns one harmless race into two leaked resources.
			rep.Failed[kindMapping]++
			st.markMappingAlive(id)
			rep.Suspect = append(rep.Suspect, fmt.Sprintf(
				"mapping %s belongs to no expected sandbox but could not be removed "+
					"(%v); something still holds it open, likely a firecracker process "+
					"from a previous noded", name, err))
			slog.Error("cannot reclaim orphaned device-mapper mapping",
				logging.KeySandbox, id, "mapping", name, logging.KeyError, err)
			continue
		}
		rep.Reclaimed[kindMapping]++
	}
}

// loops detaches loop devices this node can prove are unreferenced, and records
// which sandboxes still have one attached.
func (r *Reconciler) loops(expected map[string]bool, st *state, rep *Report) {
	devs, err := r.Host.ListLoopDevices()
	if err != nil {
		st.loopsUnknown = true
		rep.Suspect = append(rep.Suspect, fmt.Sprintf(
			"cannot list loop devices (%v); leaked devices, if any, are still held", err))
		return
	}
	for _, dev := range devs {
		// A base image's loop device is read-only and shared by every sandbox on
		// the node, and this process has no way to count its holders. Detaching one
		// that is still in use would break every guest reading from it, so bases
		// are skipped on purpose. They do not leak: acquireBase adopts an existing
		// device rather than attaching a second (GitHub #16).
		if under(r.ImageDir, dev.BackingFile) {
			rep.Kept[kindLoop]++
			continue
		}
		id, ok := sandboxIDForPath(r.BaseDir, dev.BackingFile)
		if !ok {
			// Backed by a file outside the directories this node owns, so it is
			// not bean's: snapd, lxd and Docker all hold loop devices on these
			// hosts.
			continue
		}
		if expected[id] {
			// The sandbox is supposed to be running, so the device is in use even
			// though this process did not attach it.
			rep.Kept[kindLoop]++
			st.markLoopHeld(id)
			if dev.Deleted {
				// A live sandbox whose store has been unlinked is not something
				// this code caused and not something it can fix: the file is
				// unreachable, so the sandbox cannot be checkpointed, but the
				// device still serves it and detaching would break it now.
				rep.Suspect = append(rep.Suspect, fmt.Sprintf(
					"loop device %s backs expected sandbox %s but its file %s has been "+
						"deleted; left attached because detaching would break a running "+
						"sandbox, but this sandbox can no longer be checkpointed",
					dev.Name, id, dev.BackingFile))
				slog.Warn("loop device of an expected sandbox has a deleted backing file",
					logging.KeySandbox, id, "device", dev.Name, "file", dev.BackingFile)
			}
			continue
		}
		if st.mappingsUnknown || st.mappingAlive[id] {
			// The sandbox is unexpected, but its mapping is either still standing or
			// its status is unknown because the mapping list could not be read.
			// Either way something may still be reading through this device.
			st.markLoopHeld(id)
			rep.Suspect = append(rep.Suspect, fmt.Sprintf(
				"loop device %s backs unexpected sandbox %s but its device-mapper "+
					"mapping was not removed, so it may still be in use; left attached",
				dev.Name, id))
			slog.Warn("leaving loop device of an unexpected sandbox attached",
				logging.KeySandbox, id, "device", dev.Name, "file", dev.BackingFile)
			continue
		}
		rep.Found[kindLoop]++
		slog.Info("reclaiming orphaned loop device", logging.KeySandbox, id,
			"device", dev.Name, "file", dev.BackingFile, "fileDeleted", dev.Deleted)
		if err := r.Host.DetachLoop(dev.Name); err != nil {
			rep.Failed[kindLoop]++
			st.markLoopHeld(id)
			rep.Suspect = append(rep.Suspect, fmt.Sprintf(
				"loop device %s is unreferenced but could not be detached (%v)",
				dev.Name, err))
			slog.Error("cannot reclaim orphaned loop device", logging.KeySandbox, id,
				"device", dev.Name, logging.KeyError, err)
			continue
		}
		rep.Reclaimed[kindLoop]++
	}
}

// dirs removes sandbox directories left by sandboxes that are not coming back.
//
// A directory is removable once nothing on the host references its contents:
// no mapping and no loop device. That covers both a fully torn-down orphan and
// the create that crashed before it got as far as attaching anything, which
// leaves a directory with a sparse store and no other trace.
func (r *Reconciler) dirs(expected map[string]bool, st *state, rep *Report) {
	names, err := r.Host.ListSandboxDirs()
	if err != nil {
		rep.Suspect = append(rep.Suspect, fmt.Sprintf(
			"cannot list sandbox directories under %s (%v)", r.BaseDir, err))
		return
	}
	for _, name := range names {
		if !isSandboxDirName(name) {
			continue
		}
		if expected[name] {
			rep.Kept[kindDir]++
			continue
		}
		if st.mappingsUnknown || st.loopsUnknown ||
			st.mappingAlive[name] || st.loopHeld[name] {
			// Something still references the files in here, or it cannot be shown
			// that nothing does. Deleting a store from under a live mapping is how a
			// running guest loses its filesystem, and unlike the mapping itself the
			// kernel will not refuse the unlink to save us.
			//
			// Erring towards keeping costs the disk the directory occupies until the
			// next restart, which will remove it once the references are gone.
			rep.Suspect = append(rep.Suspect, fmt.Sprintf(
				"sandbox directory %s is not expected but its files may still be open, "+
					"so it was left in place",
				filepath.Join(r.BaseDir, name)))
			slog.Warn("leaving unexpected sandbox directory in place",
				logging.KeySandbox, name, "dir", filepath.Join(r.BaseDir, name))
			continue
		}
		rep.Found[kindDir]++
		slog.Info("reclaiming orphaned sandbox directory", logging.KeySandbox, name,
			"dir", filepath.Join(r.BaseDir, name))
		if err := r.Host.RemoveSandboxDir(name); err != nil {
			rep.Failed[kindDir]++
			rep.Suspect = append(rep.Suspect, fmt.Sprintf(
				"sandbox directory %s is orphaned but could not be removed (%v)",
				filepath.Join(r.BaseDir, name), err))
			slog.Error("cannot reclaim orphaned sandbox directory",
				logging.KeySandbox, name, logging.KeyError, err)
			continue
		}
		rep.Reclaimed[kindDir]++
	}
}

// publish records the pass and states its outcome.
//
// Both the counters and the log line are unconditional, including the pass that
// found nothing. A reclaim path is only trusted if it can be seen working, and a
// metric that appears only when something was wrong cannot be alerted on: there
// is no way to tell "no orphans" from "reconciliation never ran".
func (r *Reconciler) publish(rep Report) {
	if r.Metrics != nil {
		for _, kind := range []string{kindMapping, kindLoop, kindDir} {
			r.Metrics.IncCounter("bean_node_reclaim_found_total",
				"Orphaned host resources found by startup reconciliation.",
				map[string]string{"resource": kind}, float64(rep.Found[kind]))
			r.Metrics.IncCounter("bean_node_reclaim_reclaimed_total",
				"Orphaned host resources reclaimed by startup reconciliation.",
				map[string]string{"resource": kind}, float64(rep.Reclaimed[kind]))
			r.Metrics.IncCounter("bean_node_reclaim_failures_total",
				"Orphaned host resources that could not be reclaimed and are still held.",
				map[string]string{"resource": kind}, float64(rep.Failed[kind]))
			r.Metrics.SetGauge("bean_node_reclaim_in_use",
				"Host resources matching bean's prefix left alone because a sandbox "+
					"is expected to be using them.",
				map[string]string{"resource": kind}, float64(rep.Kept[kind]))
		}
		r.Metrics.SetGauge("bean_node_reclaim_suspect",
			"Host resources that looked wrong but could not be shown to be orphans, "+
				"so were left in place. Needs a human.",
			nil, float64(len(rep.Suspect)))
	}

	slog.Info("host resource reconciliation complete",
		"mappingsFound", rep.Found[kindMapping],
		"mappingsReclaimed", rep.Reclaimed[kindMapping],
		"loopsFound", rep.Found[kindLoop],
		"loopsReclaimed", rep.Reclaimed[kindLoop],
		"dirsFound", rep.Found[kindDir],
		"dirsReclaimed", rep.Reclaimed[kindDir],
		"failures", rep.Failed[kindMapping]+rep.Failed[kindLoop]+rep.Failed[kindDir],
		"suspect", len(rep.Suspect))
	for _, s := range rep.Suspect {
		slog.Warn("host resource left in place by reconciliation", "detail", s)
	}
}

// isSandboxDirName reports whether a directory under BaseDir belongs to a
// sandbox.
//
// The runtime keeps its own state under BaseDir alongside the sandboxes — the
// unpacked snapshot cache is .snapshots, image conversion uses .work — and those
// are not sandboxes and must survive. Excluding the whole dotted namespace rather
// than listing the current names means adding another one cannot quietly make it
// a reclamation target.
func isSandboxDirName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".")
}

// sandboxIDForPath recovers the sandbox that owns a file, or false if the file is
// not inside BaseDir.
//
// This is the safety boundary for loop devices: nothing outside this directory
// can be named by the return value, so nothing outside it can be detached. The
// check is on the cleaned path rather than a string prefix, because "/var/lib/
// bean/sandboxes/../../other" has the right prefix and the wrong location.
func sandboxIDForPath(baseDir, path string) (string, bool) {
	if baseDir == "" || path == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(baseDir), filepath.Clean(path))
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// A file directly in BaseDir has one part and belongs to no sandbox; a file
	// in a sandbox directory has at least two.
	if len(parts) < 2 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return "", false
	}
	if !isSandboxDirName(parts[0]) {
		return "", false
	}
	return parts[0], true
}

// under reports whether path is inside dir.
func under(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
