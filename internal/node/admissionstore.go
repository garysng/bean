package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// admissionFile is the on-disk form of the node's admission thresholds.
//
// The node owns this file and is its only writer: the startup path reads it and
// a ConfigureAdmission push rewrites it, so the last threshold set survives a
// restart without the control plane having to re-push. This is what makes the
// thresholds a runtime *parameter* rather than a startup flag -- see
// docs/noded-design.md §7.4.
//
// Only the tunable numbers are stored. The disk floor's Path is not: the guard
// watches --base-dir, which is a startup concern (where the sandbox layers live)
// rather than a policy knob, so it is re-supplied on load instead of persisted.
// Disk is kept in MiB because that is the unit an operator provisions and reads,
// matching the old flag.
type admissionFile struct {
	MinFreeDiskMiB     int64   `json:"minFreeDiskMiB"`
	MinFreeDiskPercent float64 `json:"minFreeDiskPercent"`
	MaxMemPercent      float64 `json:"maxMemPercent"`
}

// LoadAdmission reads the admission thresholds from path and returns the guards
// they describe, with diskPath supplied for the disk floor (see admissionFile).
//
// found is false when the file does not exist, which is the fresh-node case: the
// node comes up with admission disabled until an operator provisions the file or
// pushes a config, rather than treating "no policy yet" as an error. A file that
// exists but is unparsable or describes an invalid threshold is an operator
// mistake worth stopping for, so it returns an error rather than being ignored.
func LoadAdmission(path, diskPath string) (disk DiskGuard, mem MemGuard, found bool, err error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DiskGuard{Path: diskPath}, MemGuard{}, false, nil
	}
	if err != nil {
		return DiskGuard{}, MemGuard{}, false, fmt.Errorf("read admission config %s: %w", path, err)
	}
	var f admissionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return DiskGuard{}, MemGuard{}, false, fmt.Errorf("parse admission config %s: %w", path, err)
	}
	disk = DiskGuard{
		Path:           diskPath,
		MinFreeBytes:   f.MinFreeDiskMiB << 20,
		MinFreePercent: f.MinFreeDiskPercent,
	}
	mem = MemGuard{MaxUsedPercent: f.MaxMemPercent}
	if err := disk.Validate(); err != nil {
		return DiskGuard{}, MemGuard{}, false, fmt.Errorf("admission config %s: disk: %w", path, err)
	}
	if err := mem.Validate(); err != nil {
		return DiskGuard{}, MemGuard{}, false, fmt.Errorf("admission config %s: memory: %w", path, err)
	}
	return disk, mem, true, nil
}

// SaveAdmission writes the guards' thresholds to path atomically, so a crash
// mid-write cannot leave a half-written file the next startup would reject.
//
// The Path of the disk guard is deliberately not written (see admissionFile).
// The parent directory is created if absent so a first push on a fresh node does
// not fail for want of the directory.
func SaveAdmission(path string, disk DiskGuard, mem MemGuard) error {
	f := admissionFile{
		MinFreeDiskMiB:     disk.MinFreeBytes >> 20,
		MinFreeDiskPercent: disk.MinFreePercent,
		MaxMemPercent:      mem.MaxUsedPercent,
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admission config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create admission config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write admission config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace admission config: %w", err)
	}
	return nil
}
