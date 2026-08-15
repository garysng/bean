//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"sync"
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

	// ublkAttrVolatileCache tells the kernel this device has a write-back cache, which makes
	// the block layer issue a flush when a filesystem asks for one (UBLK_ATTR_VOLATILE_CACHE).
	//
	// Without it the queue never receives UBLK_IO_OP_FLUSH, so backend.Flush is unreachable and
	// a guest's `sync` returns as soon as its writes reach the queue rather than when they are
	// on the overlay. Measured: a marker was absent from the host file 500 ms after the guest's
	// sync returned, and present within the next 500 ms.
	//
	// It describes the device honestly. The overlay is a host file whose pages are not durable
	// until fsync, which is exactly what a volatile cache is, and the guest is the only party
	// that knows which writes it needs ordered.
	//
	// The value is from the kernel's own header rather than counted off: 1 << 1 is
	// UBLK_ATTR_ROTATIONAL, so an off-by-one bit here would tell the kernel this is a spinning
	// disk and say nothing about the cache.
	ublkAttrVolatileCache = 1 << 2

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

	ctrl *ublkControl
	// cdev is the char device: the queue's descriptor mapping and every data transfer
	// go through it, so it outlives attach and is closed by detach.
	cdev *os.File
	// queue serves this device's IO. Stopped between stopDevice and deleteDevice --
	// see detach for why that order is the only safe one.
	queue *ublkQueue
}

// ublkControl is a handle on /dev/ublk-control plus the ring commands go through.
//
// One per node rather than one per device: the control device is a singleton and the
// commands on it are infrequent, so a shared handle avoids an fd and a ring per sandbox.
type ublkControl struct {
	f *os.File

	// mu serialises use of the ring below.
	//
	// The ring is not a concurrency primitive: submitting means writing the SQE at the
	// tail and then advancing it, and reading a completion means taking the one at the
	// head. Two callers doing that at once interleave their writes and take each
	// other's results.
	//
	// Measured, and the symptom named nothing useful: at 256 concurrent creates, 255
	// failed with "this kernel's ublk lacks UBLK_F_USER_COPY (features=0x0)" on a kernel
	// that had answered 0x1fe seconds earlier. GET_FEATURES was reading a completion
	// belonging to somebody else's ADD_DEV, so the error accused the kernel of a missing
	// feature when the fault was entirely here.
	//
	// Serialising costs nothing that matters: these commands happen twice per sandbox
	// and each takes tens of microseconds, against a create that takes hundreds of
	// milliseconds. The per-queue rings on the IO path are separate and stay
	// independent, which is where concurrency actually has to hold.
	mu   sync.Mutex
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
	// Held across submit and completion, not just the submit: the result has to be read
	// before another caller can advance the head past it.
	c.mu.Lock()
	defer c.mu.Unlock()
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
	// Declared as having a volatile write cache so the guest's flushes reach the queue. The
	// overlay is a host file, so a write is not durable until the backend fsyncs it, and that
	// is what this attribute says. Left unset, the guest sees "write through", never sends a
	// flush, and its `sync` is not a durability point at all.
	p.Basic.Attrs = ublkAttrVolatileCache

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
