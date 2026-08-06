//go:build linux

package reclaim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/garysng/bean/internal/node/image"
)

// LinuxHost inspects and modifies the host with dmsetup and losetup, the same
// tools the device-mapper provider uses to create what is being reclaimed.
type LinuxHost struct {
	// BaseDir is the sandbox base directory. Removals are confined to it: the
	// caller passes a name, never a path, so a bug elsewhere cannot direct a
	// delete outside this directory.
	BaseDir string
}

// NewLinuxHost builds a host bound to one sandbox directory. It returns the
// interface rather than the concrete type so the non-Linux stub can share the
// signature and hand back a genuinely nil Host.
func NewLinuxHost(baseDir string) Host {
	return &LinuxHost{BaseDir: baseDir}
}

// ListDMNames returns every mapping on the host.
//
// "No devices found" is reported on stdout with a zero exit status by some
// versions and as an error by others, so the output is parsed permissively and an
// unparseable line is skipped rather than failing the pass.
func (h *LinuxHost) ListDMNames() ([]string, error) {
	out, err := exec.Command("dmsetup", "ls").Output()
	if err != nil {
		return nil, fmt.Errorf("reclaim: dmsetup ls: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "No devices") {
			continue
		}
		// Each line is "<name>\t(major, minor)"; the name is everything up to the
		// first whitespace, and mapping names cannot contain whitespace.
		name := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t'
		})
		if len(name) == 0 {
			continue
		}
		names = append(names, name[0])
	}
	return names, nil
}

// RemoveDM tears down one mapping.
//
// --retry is used because a device can be momentarily busy while the kernel
// finishes with it, and a single attempt turns that into a spurious failure. It
// does not force: a device held open by a live process still fails, which is the
// outcome the caller depends on to avoid destroying a running sandbox.
func (h *LinuxHost) RemoveDM(name string) error {
	if _, ok := image.SandboxIDFromDMName(name); !ok {
		// Belt and braces. The caller already filters by prefix; this makes a
		// future caller that forgets fail loudly rather than remove a stranger's
		// mapping.
		return fmt.Errorf("reclaim: refusing to remove mapping %q: not bean's", name)
	}
	err := run("dmsetup", "remove", "--retry", name)
	if err != nil && dmAlreadyGone(err) {
		// The mapping disappeared between ListDMNames and now, which is the common
		// case rather than a rare one: a sandbox destroying itself concurrently with
		// a reconcile pass removes its own mapping, and the pass then finds it
		// missing. That is the outcome this call wanted.
		//
		// Reporting it as a failure was measured to cascade. The caller treats a
		// failed removal as "something still holds it open" and marks the sandbox's
		// mapping alive, which then blocks reclaiming its loop device and its
		// directory -- so one already-completed removal leaks the two resources
		// behind it. A 300-sandbox burst produced 109 of these.
		return nil
	}
	return err
}

// dmAlreadyGone reports whether a dmsetup failure means the mapping was not there.
//
// Matched on message text because dmsetup exits 1 for every error, so the exit code
// cannot distinguish "already gone" from "still busy" -- and those two need opposite
// handling. Narrow on purpose: only the kernel's ENXIO/ENODEV wording for a missing
// device counts, so a busy device, a permission problem, or a missing dmsetup all
// still fail loudly.
func dmAlreadyGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such device or address") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "device does not exist")
}

// ListLoopDevices returns every loop device with its backing file.
func (h *LinuxHost) ListLoopDevices() ([]LoopDevice, error) {
	// --raw with an explicit column list keeps the parse independent of the
	// default column set, which varies between util-linux versions.
	out, err := exec.Command("losetup", "--noheadings", "--raw",
		"--output", "NAME,BACK-FILE", "--list").Output()
	if err != nil {
		return nil, fmt.Errorf("reclaim: losetup --list: %w", err)
	}
	var devs []LoopDevice
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, back, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		devs = append(devs, parseLoopLine(name, back))
	}
	return devs, nil
}

// parseLoopLine splits the backing file from the " (deleted)" suffix the kernel
// appends once the file has been unlinked.
//
// That suffix is the whole signal for the case in GitHub #17: a device holding a
// deleted cow.img is unambiguously garbage, since nothing can ever open the file
// again, yet its blocks stay allocated for the life of the host.
func parseLoopLine(name, back string) LoopDevice {
	back = strings.TrimSpace(back)
	dev := LoopDevice{Name: strings.TrimSpace(name)}
	if trimmed, ok := strings.CutSuffix(back, " (deleted)"); ok {
		dev.Deleted = true
		back = trimmed
	}
	dev.BackingFile = back
	return dev
}

// DetachLoop releases one loop device. It does not pass --detach-all or force
// anything: the kernel refuses to detach a device that is still open, and that
// refusal is a safety property here rather than an inconvenience.
func (h *LinuxHost) DetachLoop(dev string) error {
	if !strings.HasPrefix(dev, "/dev/loop") {
		return fmt.Errorf("reclaim: refusing to detach %q: not a loop device", dev)
	}
	return run("losetup", "-d", dev)
}

// ListSandboxDirs lists the entries directly under the sandbox base directory. A
// missing directory is not an error: a node that has never created a sandbox has
// nothing to reconcile.
func (h *LinuxHost) ListSandboxDirs() ([]string, error) {
	entries, err := os.ReadDir(h.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reclaim: read %s: %w", h.BaseDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// RemoveSandboxDir removes one sandbox directory.
//
// The argument is a bare name and is rejected if it is anything else, so the path
// that gets deleted is always one level under BaseDir. A recursive delete driven
// by a value from the host deserves that check even though the caller derived the
// name from a directory listing.
func (h *LinuxHost) RemoveSandboxDir(name string) error {
	if h.BaseDir == "" {
		return fmt.Errorf("reclaim: no base directory")
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." ||
		strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("reclaim: refusing to remove sandbox directory %q: "+
			"not a single path element", name)
	}
	return os.RemoveAll(filepath.Join(h.BaseDir, name))
}

// run folds stderr into the error: dmsetup and losetup explain themselves there,
// and "exit status 1" from either says nothing about whether the device was busy
// or absent.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
