//go:build linux

package image

import "unsafe"

// The ublk ABI, transcribed from include/uapi/linux/ublk_cmd.h.
//
// Written out here rather than pulled from a binding library because there is no
// maintained Go one, and the surface bean needs is small: add a device, set its
// parameters, start it, stop it, delete it. The risk in transcribing a uapi header is
// getting an encoding wrong and having the kernel reject a command with EINVAL, which
// names nothing -- so every constant below is derived by the same arithmetic the header
// uses rather than pasted as a literal, and the tests check the two the kernel
// documents.
//
// Why this exists at all: tcmu presents an overlaybd image as a block device through
// configfs and a single netlink socket, and that socket is the serialisation measured at
// 4.0s to tear down 128 devices, unchanged between kernels 5.15 and 6.8. ublk replaces
// the transport with io_uring, where each device has its own submission queue.

// ioctl direction bits, from include/uapi/asm-generic/ioctl.h.
const (
	iocWrite = 1
	iocRead  = 2

	iocNRShift   = 0
	iocTypeShift = 8
	iocSizeShift = 16
	iocDirShift  = 30
)

// ublksrvCtrlCmd is struct ublksrv_ctrl_cmd. Its size is part of every command's
// encoding, so a field added in the wrong place changes the command number and the
// kernel rejects it as unknown rather than as malformed.
type ublksrvCtrlCmd struct {
	DevID      uint32
	QueueID    uint16
	Len        uint16
	Addr       uint64
	Data       [1]uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

// ioc encodes an ioctl number the way the kernel's _IOWR macro does.
//
// A function rather than pre-computed literals so the size term cannot drift from the
// struct it describes: unsafe.Sizeof is evaluated against the definition above, so a
// change to the struct changes the command numbers exactly as it would in C.
func ioc(dir, typ, nr, size uintptr) uint32 {
	return uint32(dir<<iocDirShift | size<<iocSizeShift | typ<<iocTypeShift | nr<<iocNRShift)
}

// ublkCmdWR builds an _IOWR('u', nr, struct ublksrv_ctrl_cmd) command, and ublkCmdR an
// _IOR one.
//
// The direction is per command in ublk_cmd.h and is not decorative: it is part of the
// command number, so using _IOWR where the header says _IOR produces a word the kernel
// does not recognise. Measured -- GET_FEATURES built as _IOWR returned EINVAL, which
// reads exactly like a malformed argument and sent me looking at the struct instead.
func ublkCmdWR(nr uintptr) uint32 {
	return ioc(iocRead|iocWrite, 'u', nr, unsafe.Sizeof(ublksrvCtrlCmd{}))
}

func ublkCmdR(nr uintptr) uint32 {
	return ioc(iocRead, 'u', nr, unsafe.Sizeof(ublksrvCtrlCmd{}))
}

// The control commands bean uses, each with the direction ublk_cmd.h gives it. The nr
// values are from the header; the full command word is computed, not written down.
var (
	ublkCmdGetQueueAffinity = ublkCmdR(0x01)  // _IOR
	ublkCmdGetDevInfo       = ublkCmdR(0x02)  // _IOR
	ublkCmdAddDev           = ublkCmdWR(0x04) // _IOWR
	ublkCmdDelDev           = ublkCmdWR(0x05) // _IOWR
	ublkCmdStartDev         = ublkCmdWR(0x06) // _IOWR
	ublkCmdStopDev          = ublkCmdWR(0x07) // _IOWR
	ublkCmdSetParams        = ublkCmdWR(0x08) // _IOWR
	ublkCmdGetParams        = ublkCmdR(0x09)  // _IOR
	ublkCmdGetFeatures      = ublkCmdR(0x13)  // _IOR
	ublkCmdUpdateSize       = ublkCmdWR(0x15) // _IOWR
)

// Feature bits from ublk_cmd.h. Only the ones bean checks are listed: a flag nobody
// reads is a flag nobody keeps correct.
const (
	// ublkFUserCopy has the driver copy data through the daemon's buffers rather than
	// mapping them, which is what lets a userspace server without zero-copy support
	// serve a device at all.
	ublkFUserCopy = 1 << 7
	// ublkFUpdateSize allows UBLK_U_CMD_UPDATE_SIZE after the device is running.
	ublkFUpdateSize = 1 << 10
)

// ublkIOCmd is struct ublksrv_io_cmd, the per-request message on a queue's io_uring.
type ublkIOCmd struct {
	QID    uint16
	Tag    uint16
	Result int32
	Addr   uint64
}

// The io commands, _IOWR('u', nr, struct ublksrv_io_cmd).
func ublkIOCmdNum(nr uintptr) uint32 {
	return ioc(iocRead|iocWrite, 'u', nr, unsafe.Sizeof(ublkIOCmd{}))
}

var (
	ublkIOFetchReq       = ublkIOCmdNum(0x20)
	ublkIOCommitAndFetch = ublkIOCmdNum(0x21)
	ublkIONeedGetData    = ublkIOCmdNum(0x22)
)

// Request operations from struct ublksrv_io_desc's op_flags, masked with 0xff.
const (
	ublkIOOpRead      = 0
	ublkIOOpWrite     = 1
	ublkIOOpFlush     = 2
	ublkIOOpDiscard   = 3
	ublkIOOpWriteSame = 4
	ublkIOOpWriteZero = 5

	ublkIOOpMask = 0xff
)

// ublkIODesc is struct ublksrv_io_desc: what the kernel puts in the shared mmap to
// describe one request.
type ublkIODesc struct {
	OpFlags     uint32
	NRSectors   uint32
	StartSector uint64
	Addr        uint64
}
