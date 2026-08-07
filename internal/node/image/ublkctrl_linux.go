//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ublk device lifecycle: add, configure, start, stop, delete.
//
// Every step is a uring_cmd on /dev/ublk-control. The ordering is not negotiable and the
// kernel does not explain violations: parameters must be set before the device starts,
// because start is what creates the gendisk and it reads the parameters then. Setting
// them afterwards succeeds and changes nothing.

// ublkParams is struct ublk_params. Its Len field is the wire version marker: the driver
// compares it against its own sizeof and may return a shorter one from GET_PARAMS, so it
// is set from the Go struct rather than hardcoded.
type ublkParams struct {
	Len   uint32
	Types uint32

	Basic struct {
		Attrs            uint32
		LogicalBSShift   uint8
		PhysicalBSShift  uint8
		IOOptShift       uint8
		IOMinShift       uint8
		MaxSectors       uint32
		ChunkSectors     uint32
		DevSectors       uint64
		VirtBoundaryMask uint64
	}
	Discard struct {
		DiscardAlignment      uint32
		DiscardGranularity    uint32
		MaxDiscardSectors     uint32
		MaxWriteZeroesSectors uint32
		MaxDiscardSegments    uint16
		Reserved0             uint16
	}
	Devt struct {
		CharMajor, CharMinor, DiskMajor, DiskMinor uint32
	}
	Zoned struct {
		MaxOpenZones         uint32
		MaxActiveZones       uint32
		MaxZoneAppendSectors uint32
		Reserved             [20]uint8
	}
}

const (
	ublkParamTypeBasic = 1 << 0

	// ublkDevIDAny asks the kernel to allocate a device id.
	//
	// Chosen over picking one because two concurrent creates racing for the same id is
	// exactly the class of bug the tcmu path already produced (see setSerial): the
	// kernel is the only party that can allocate without a race.
	ublkDevIDAny = 0xffffffff

	// ublkQueueIDNone marks a command that is not about a queue. It must be set: a
	// zero queue_id names queue 0, and the kernel then validates against a queue that
	// may not exist.
	ublkQueueIDNone = 0xffff
)

// ublkDevice is one attached ublk device.
type ublkDevice struct {
	// DevID is the kernel-allocated id, so /dev/ublkb<DevID> is the block device and
	// /dev/ublkc<DevID> the char device the server binds.
	DevID uint32
	// Device is the block device path a VM attaches.
	Device string

	// Size is what the device reports, so a caller can check it matches what it asked
	// for rather than trusting the sequence silently worked.
	Size int64
}

// ublkControl is a handle on /dev/ublk-control plus the ring commands go through.
//
// One per node rather than one per device: the control device is a singleton and the
// commands on it are infrequent, so a shared handle avoids an fd and a ring per sandbox.
type ublkControl struct {
	f    *os.File
	ring *ioURing
}

// openUblkControl opens the control device.
//
// The error distinguishes the three ways this fails, because they need different
// responses: no kernel support (upgrade), no module (modprobe), no permission (run as
// root or fix udev). A single "cannot open" would send an operator to the wrong one.
func openUblkControl() (*ublkControl, error) {
	f, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return nil, fmt.Errorf("image: /dev/ublk-control is absent; ublk needs "+
				"kernel 6.0 or later with ublk_drv loaded (modprobe ublk_drv): %w", err)
		case os.IsPermission(err):
			return nil, fmt.Errorf("image: cannot open /dev/ublk-control; it is "+
				"root-only unless a udev rule says otherwise: %w", err)
		default:
			return nil, fmt.Errorf("image: open /dev/ublk-control: %w", err)
		}
	}
	// Depth 8: control commands are issued one at a time and waited on, so the ring
	// only needs room for the one in flight plus slack.
	ring, err := newIOURing(8)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("image: io_uring for ublk control: %w", err)
	}
	return &ublkControl{f: f, ring: ring}, nil
}

func (c *ublkControl) Close() error {
	var errs []error
	if c.ring != nil {
		errs = append(errs, c.ring.Close())
	}
	if c.f != nil {
		errs = append(errs, c.f.Close())
	}
	return errors.Join(errs...)
}

// cmd issues one control command and turns the kernel's negative result into an error.
func (c *ublkControl) cmd(op uint32, h *ublksrvCtrlCmd) error {
	payload := (*[unsafe.Sizeof(ublksrvCtrlCmd{})]byte)(unsafe.Pointer(h))[:]
	res, err := c.ring.uringCmd(int(c.f.Fd()), op, payload)
	if err != nil {
		return err
	}
	if res < 0 {
		return fmt.Errorf("ublk command %#x: %w", op, unix.Errno(-res))
	}
	return nil
}

// Features reports what this kernel's ublk supports.
func (c *ublkControl) Features() (uint64, error) {
	var features uint64
	h := ublksrvCtrlCmd{
		QueueID: ublkQueueIDNone,
		Addr:    uint64(uintptr(unsafe.Pointer(&features))),
		Len:     uint16(unsafe.Sizeof(features)),
	}
	if err := c.cmd(ublkCmdGetFeatures, &h); err != nil {
		return 0, err
	}
	return features, nil
}

// addDevice creates a device and returns the id the kernel allocated.
func (c *ublkControl) addDevice(queues, depth uint16, ioBufBytes uint32, flags uint64) (*ublksrvCtrlDevInfo, error) {
	info := ublksrvCtrlDevInfo{
		NRHWQueues:    queues,
		QueueDepth:    depth,
		MaxIOBufBytes: ioBufBytes,
		DevID:         ublkDevIDAny,
		Flags:         flags,
	}
	h := ublksrvCtrlCmd{
		DevID:   ublkDevIDAny,
		QueueID: ublkQueueIDNone,
		Addr:    uint64(uintptr(unsafe.Pointer(&info))),
		Len:     uint16(unsafe.Sizeof(info)),
	}
	if err := c.cmd(ublkCmdAddDev, &h); err != nil {
		return nil, err
	}
	return &info, nil
}

// setParams describes the block device the kernel should present.
//
// Must happen before startDevice: start creates the gendisk and reads these then, so a
// later call succeeds and changes nothing -- a device with a zero size that reports no
// error anywhere.
func (c *ublkControl) setParams(devID uint32, sizeBytes int64) error {
	p := ublkParams{
		Len:   uint32(unsafe.Sizeof(ublkParams{})),
		Types: ublkParamTypeBasic,
	}
	// 512-byte logical blocks with 4 KiB physical, which is what a virtio-blk guest
	// expects and what the ext4 in bean's images is formatted for.
	p.Basic.LogicalBSShift = 9
	p.Basic.PhysicalBSShift = 12
	p.Basic.IOOptShift = 12
	p.Basic.IOMinShift = 9
	p.Basic.MaxSectors = 256 << (10 - 9) // 256 KiB in sectors
	p.Basic.DevSectors = uint64(sizeBytes >> 9)

	h := ublksrvCtrlCmd{
		DevID:   devID,
		QueueID: ublkQueueIDNone,
		Addr:    uint64(uintptr(unsafe.Pointer(&p))),
		Len:     uint16(unsafe.Sizeof(p)),
	}
	return c.cmd(ublkCmdSetParams, &h)
}

// startDevice creates the block device. pid is the process that will serve its IO, which
// the kernel records and requires to be alive.
func (c *ublkControl) startDevice(devID uint32, pid int) error {
	h := ublksrvCtrlCmd{
		DevID:   devID,
		QueueID: ublkQueueIDNone,
		Data:    [1]uint64{uint64(pid)},
	}
	return c.cmd(ublkCmdStartDev, &h)
}

// stopDevice removes the block device but keeps the id allocated.
func (c *ublkControl) stopDevice(devID uint32) error {
	h := ublksrvCtrlCmd{DevID: devID, QueueID: ublkQueueIDNone}
	return c.cmd(ublkCmdStopDev, &h)
}

// deleteDevice releases the id.
func (c *ublkControl) deleteDevice(devID uint32) error {
	h := ublksrvCtrlCmd{DevID: devID, QueueID: ublkQueueIDNone}
	return c.cmd(ublkCmdDelDev, &h)
}
