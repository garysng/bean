//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Exposing an overlaybd device to a VM means asking the kernel's SCSI target
// subsystem to present it as a block device, and that is driven entirely by
// writing files under configfs. There is no library and no ioctl: the sequence of
// writes _is_ the API.
//
// Two orderings in here are load-bearing, both learned from hardware rather than
// documentation (decisions.md 3.1). Getting either wrong produces no error --
// configfs still reports enable=1 and ACTIVATED -- so they are enforced in code
// with the reasoning attached, because a comment in a design document does not
// survive the next refactor.

const (
	configfsCore     = "/sys/kernel/config/target/core"
	configfsLoopback = "/sys/kernel/config/target/loopback"
	// hbaName is the TCMU backstore HBA. The number is arbitrary but must be
	// stable, since it is part of every path underneath.
	hbaName = "user_999"
)

// tcmuDevice is one attached overlaybd device.
type tcmuDevice struct {
	// Name identifies the backstore and the loopback target, derived from the
	// sandbox id so a leaked object is traceable to its owner.
	Name string
	// Device is the block device path, e.g. /dev/sdb.
	Device string
	// wwn is the loopback target's name, needed to unwind it.
	wwn string
}

// attach presents an overlaybd config as a block device.
//
// configPath is the per-device JSON overlaybd reads; sizeBytes is the device's
// virtual size, which the kernel needs before overlaybd has opened anything.
//
// serial must be unique per device on this host. It is a parameter rather than
// derived here so the caller cannot forget it: see setSerial for what happens
// otherwise.
func attachTCMU(name, configPath, serial string, sizeBytes int64) (dev *tcmuDevice, err error) {
	if name == "" || configPath == "" || serial == "" {
		return nil, errors.New("image: tcmu attach needs a name, config and serial")
	}
	// Rejected rather than sanitised. The kernel keeps only the hex digits of a
	// serial when it builds the WWID, so a serial carrying other characters is
	// silently shortened -- and two different ones can shorten to the same value,
	// which is the multipathd merge that serves another sandbox's data. Quietly
	// rewriting the caller's serial here would hide which value actually reached
	// the kernel.
	if hexSerial(serial) != strings.ToLower(serial) {
		return nil, fmt.Errorf("image: serial %q must be hex digits only "+
			"(the kernel drops the rest when building the WWID, and two serials "+
			"that reduce to the same digits are merged by multipathd)", serial)
	}

	var undo []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()

	if err := os.MkdirAll(filepath.Join(configfsCore, hbaName), 0o755); err != nil {
		return nil, fmt.Errorf("image: create tcmu hba: %w", err)
	}

	// Step 1: the backstore. Creating the directory registers the device with
	// target_core_user; the control writes then tell it what to serve.
	base := filepath.Join(configfsCore, hbaName, name)
	if err := os.Mkdir(base, 0o755); err != nil {
		return nil, fmt.Errorf("image: create backstore %s: %w", name, err)
	}
	undo = append(undo, func() { os.Remove(base) })

	// dev_config points overlaybd at its config file; the "overlaybd/" prefix is
	// how the daemon recognises a device as its own.
	control := fmt.Sprintf("dev_config=overlaybd/%s,dev_size=%d", configPath, sizeBytes)
	if err := writeAttr(filepath.Join(base, "control"), control); err != nil {
		return nil, err
	}

	// Step 2: the serial, before the device is enabled. See setSerial.
	if err := setSerial(base, serial); err != nil {
		return nil, err
	}

	// A failure here is usually the daemon refusing the config rather than configfs
	// refusing the write: an unsealed lower layer, or a path it cannot read, surfaces
	// only as ENOENT on this write with the reason in the daemon's log.
	if err := writeAttr(filepath.Join(base, "enable"), "1"); err != nil {
		return nil, fmt.Errorf("image: enable backstore (is overlaybd-tcmu running, "+
			"and are all lower layers sealed?): %w", err)
	}
	// Undone by removing the directory, not by writing enable=0: the kernel rejects
	// that write ("only valid value is 1").
	undo = append(undo, func() { _ = os.Remove(base) })

	// Step 3: the loopback fabric, which is what turns a backstore into something
	// the local SCSI layer scans. The naa address is arbitrary but must be unique.
	wwn := "naa.5001405" + shortHash(name)
	tpgt := filepath.Join(configfsLoopback, wwn, "tpgt_1")
	if err := os.MkdirAll(tpgt, 0o755); err != nil {
		return nil, fmt.Errorf("image: create loopback tpgt: %w", err)
	}
	undo = append(undo, func() { os.RemoveAll(filepath.Join(configfsLoopback, wwn)) })

	// Step 4: the nexus MUST exist before the LUN is linked.
	//
	// The SCSI host scans for LUNs when the fabric registers. Registration happens
	// on the nexus write, and a LUN linked afterwards is never scanned -- writing
	// the nexus later does not trigger a rescan. The device simply never appears,
	// while configfs looks entirely healthy: enable=1, info says ACTIVATED, and
	// overlaybd's own result file says success. Nothing anywhere reports a
	// problem, which is why this ordering is a comment this long.
	if err := writeAttr(filepath.Join(tpgt, "nexus"), wwn); err != nil {
		return nil, fmt.Errorf("image: set loopback nexus: %w", err)
	}

	// Step 5: now the LUN.
	lun := filepath.Join(tpgt, "lun", "lun_0")
	if err := os.MkdirAll(lun, 0o755); err != nil {
		return nil, fmt.Errorf("image: create lun: %w", err)
	}
	if err := os.Symlink(base, filepath.Join(lun, "virtual_scsi_port")); err != nil {
		return nil, fmt.Errorf("image: link lun to backstore: %w", err)
	}

	device, err := waitForDevice(serial)
	if err != nil {
		return nil, err
	}

	return &tcmuDevice{Name: name, Device: device, wwn: wwn}, nil
}

// setSerial gives the device a unique SCSI serial number.
//
// TCMU does not provide one by default, so every device reports WWID 36001405
// followed by zeros. multipathd sees two devices with identical WWIDs, concludes
// they are two paths to one LUN, and merges them into a single mpath device.
//
// The consequence is not an error. It is **reading another image's data** -- the
// merged device serves whichever path multipathd picked. On top of that the
// original device becomes busy and cannot be mounted, and the error it gives for
// that ("already mounted or mount point busy") points nowhere near the cause.
//
// Written before enable=1 because the serial is read when the device comes up;
// setting it afterwards leaves the wrong WWID already registered.
func setSerial(base, serial string) error {
	path := filepath.Join(base, "wwn", "vpd_unit_serial")
	if err := writeAttr(path, serial); err != nil {
		return fmt.Errorf("image: set unit serial (without it multipathd merges devices): %w", err)
	}
	return nil
}

// waitForDevice resolves an attached device to its block device path, identifying
// it by the serial this process chose.
//
// Polled rather than awaited: the device appears when the SCSI layer scans the
// fabric, which is asynchronous with the configfs write returning.
func waitForDevice(serial string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if name, ok := blockDeviceForSerial(serial); ok {
			return "/dev/" + name, nil
		}
		if time.Now().After(deadline) {
			// Specific about the likely cause: a healthy-looking configfs tree
			// with no device is nearly always the nexus ordering.
			return "", errors.New("image: tcmu device did not appear " +
				"(configfs looks healthy when the LUN was linked before the nexus)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockDeviceForSerial finds the block device whose SCSI WWID encodes this serial.
//
// The kernel builds a TCMU WWID as "naa.6001405" followed by the *hex-digit
// characters of the unit serial*, zero-padded -- measured on hardware across six
// serials:
//
//	bean-aaa       -> naa.6001405 beaaaa 0000...
//	bean-bbb       -> naa.6001405 beabbb 0000...
//	bean-diag2     -> naa.6001405 beada2 0000...
//
// so "bean-aaa" contributes b,e,a,a,a,a and the rest of the string is dropped. That
// is why serials must be hex-only: see hexSerial.
//
// Three sysfs values that look like they would answer this do not, all confirmed on
// hardware:
//
//   - `udev_path` on the backstore stays empty.
//   - the SCSI model reads "TCMUdevice" for every TCMU device, so matching it
//     returns an arbitrary one of this host's overlaybd devices -- the same class of
//     bug as the multipathd merge: a working device belonging to another sandbox.
//   - `statistics/scsi_port/dev` is NOT the tcm_loop adapter number. A LUN reporting
//     dev=26 was served by tcm_loop_adapter_24; it is a global port counter. An
//     earlier attempt matched on it and appeared to work only because two devices
//     attached in one batch happened to line up.
//
// The adapter directories carry no wwn at all (their uevent is just DRIVER=tcm_loop),
// so walking down from the target is not available either.
func blockDeviceForSerial(serial string) (string, bool) {
	want := hexSerial(serial)
	if want == "" {
		return "", false
	}
	entries, err := os.ReadDir("/sys/class/scsi_device")
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		dev := filepath.Join("/sys/class/scsi_device", e.Name(), "device")
		raw, err := os.ReadFile(filepath.Join(dev, "wwid"))
		if err != nil {
			continue
		}
		wwid := strings.ToLower(strings.TrimSpace(string(raw)))
		// Anchored on the vendor prefix so the comparison is against the
		// serial-derived part rather than matching anywhere in the string.
		const prefix = "naa.6001405"
		if !strings.HasPrefix(wwid, prefix) ||
			!strings.HasPrefix(wwid[len(prefix):], want) {
			continue
		}
		names, err := os.ReadDir(filepath.Join(dev, "block"))
		if err != nil || len(names) == 0 {
			continue
		}
		return names[0].Name(), true
	}
	return "", false
}

// detach unwinds an attached device.
//
// Order is the reverse of attach and matters as much: removing the backstore while
// the LUN still references it leaves the kernel holding a device nothing can
// remove without a reboot. Every step runs even if an earlier one fails, because a
// partial teardown leaks a configfs object that blocks the next attach under the
// same name.
func (d *tcmuDevice) detach() error {
	var errs []error

	tpgt := filepath.Join(configfsLoopback, d.wwn, "tpgt_1")
	if err := os.Remove(filepath.Join(tpgt, "lun", "lun_0", "virtual_scsi_port")); err != nil &&
		!os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("unlink lun: %w", err))
	}
	if err := os.Remove(filepath.Join(tpgt, "lun", "lun_0")); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove lun: %w", err))
	}
	if err := os.RemoveAll(filepath.Join(configfsLoopback, d.wwn)); err != nil {
		errs = append(errs, fmt.Errorf("remove loopback: %w", err))
	}

	// No enable=0 here. The kernel rejects it -- "For dev_enable ops, only valid
	// value is 1" -- so a backstore is torn down by removing its directory, and the
	// write would only add a spurious EINVAL to every teardown.
	base := filepath.Join(configfsCore, hbaName, d.Name)
	if err := os.Remove(base); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove backstore: %w", err))
	}
	return errors.Join(errs...)
}

// writeAttr writes one configfs attribute.
//
// Opened without O_TRUNC and written in a single call: configfs attributes are
// not files with contents to replace, they are setters that parse one write. A
// buffered writer that split a value across two writes would have the kernel
// parse each half.
func writeAttr(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("image: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(value); err != nil {
		return fmt.Errorf("image: write %s=%q: %w", filepath.Base(path), value, err)
	}
	return nil
}

// tcmuAvailable reports whether this host can attach overlaybd devices.
func tcmuAvailable() error {
	if _, err := os.Stat(configfsCore); err != nil {
		return errors.New("image: configfs target unavailable (modprobe target_core_user)")
	}
	// The loopback fabric is what turns a backstore into a scanned SCSI device.
	// tcm_loop creates its configfs group on demand -- attach() does the MkdirAll
	// (see step 3) -- so the group is absent on a capable host that has not attached
	// a device yet. Gating on the group existing therefore rejected a host that could
	// serve overlaybd fine; the loaded module is the signal that the group can be
	// created, and the configfs path is accepted too for a host where a prior attach
	// already materialised it.
	if _, err := os.Stat("/sys/module/tcm_loop"); err == nil {
		return nil
	}
	if _, err := os.Stat(configfsLoopback); err != nil {
		return errors.New("image: loopback fabric unavailable (modprobe tcm_loop)")
	}
	return nil
}
