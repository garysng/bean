//go:build linux

package image

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A minimal io_uring, enough to submit ublk's uring_cmd and read the result.
//
// Hand-rolled rather than taken from a library for the same reason the ABI above is:
// what ublk needs is one SQE shape (IORING_OP_URING_CMD) and blocking completion, and
// every Go io_uring package either wraps liburing through cgo -- which CGO_ENABLED=0
// rules out -- or brings a polling runtime this has no use for.
//
// Deliberately not a general-purpose ring. There is no SQPOLL, no registered buffers,
// no batching: a ublk control command happens once per sandbox create and destroy, so
// the cost that matters is correctness, not submission overhead.

// io_uring syscall numbers are in x/sys/unix; the structures are not, so they are here.
const (
	ioringOpUringCmd = 46

	ioringEnterGetEvents = 1

	// ioringSetupSQE128 doubles the SQE to 128 bytes, which enlarges the inline
	// command area from 16 bytes to 80.
	//
	// Required, not an optimisation: struct ublksrv_ctrl_cmd is 32 bytes and does not
	// fit the default SQE's 16-byte area. Submitting it there was measured to return
	// EINVAL from GET_FEATURES -- the kernel reads a truncated command and rejects it,
	// naming nothing. agentenv's Rust client uses UringCmd80 for the same reason.
	ioringSetupSQE128 = 1 << 10

	ioringOffSQRing = 0
	ioringOffCQRing = 0x8000000
	ioringOffSQES   = 0x10000000
)

// ioSQRingOffsets is struct io_sqring_offsets.
type ioSQRingOffsets struct {
	Head, Tail, RingMask, RingEntries, Flags, Dropped, Array uint32
	Resv1                                                    uint32
	Resv2                                                    uint64
}

// ioCQRingOffsets is struct io_cqring_offsets.
type ioCQRingOffsets struct {
	Head, Tail, RingMask, RingEntries, Overflow, CQEs, Flags uint32
	Resv1                                                    uint32
	Resv2                                                    uint64
}

// ioUringParams is struct io_uring_params.
type ioUringParams struct {
	SQEntries, CQEntries, Flags, SQThreadCPU, SQThreadIdle uint32
	Features, WQFD                                         uint32
	Resv                                                   [3]uint32
	SQOff                                                  ioSQRingOffsets
	CQOff                                                  ioCQRingOffsets
}

// ioUringSQE is struct io_uring_sqe. The last 16 bytes are the uring_cmd payload for
// commands whose data fits inline, which is what ublk's control commands use.
type ioUringSQE struct {
	Opcode uint8
	Flags  uint8
	IOPrio uint16
	FD     int32
	// CmdOp is at offset 8, inside the union the kernel shares with off/addr2. This
	// is where IORING_OP_URING_CMD's command number goes.
	//
	// Getting this wrong is what made every ublk command return EINVAL. The command
	// number was being written at offset 28 instead, which the kernel reads as
	// uring_cmd_flags -- and io_uring_cmd_prep rejects any value outside
	// IORING_URING_CMD_MASK. Six variations of the command's contents were tried
	// against that, all failing identically, because none of them was the problem.
	CmdOp uint32
	// Pad1 must be zero: io_uring_cmd_prep starts with `if (sqe->__pad1) return
	// -EINVAL`, so a stale value here fails the request before the driver sees it.
	Pad1        uint32
	Addr        uint64
	Len         uint32
	CmdFlags    uint32 // uring_cmd_flags; must be 0 or IORING_URING_CMD_FIXED
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFDIn  int32
	// Cmd is the inline uring_cmd area: 16 bytes in the base SQE plus the 64 the
	// doubled SQE adds, so 80.
	//
	// It starts at offset 48, where the kernel's union puts addr3 and cmd in the same
	// place -- so there is no separate Addr3 field. Declaring one made this struct 136
	// bytes, which put every entry after the first at the wrong offset in the mapped
	// array; the kernel read a malformed command and returned EINVAL, naming nothing.
	//
	// The total must be exactly 128 bytes under IORING_SETUP_SQE128, and getting that
	// wrong is silent. Measured: an earlier version of this struct was 136 bytes, which
	// made every entry after the first land at the wrong offset in the mapped array --
	// the kernel then read a malformed command and returned EINVAL, naming nothing.
	// The assertion below is what keeps that from recurring.
	Cmd [80]byte
}

// The kernel's SQE128 layout is exactly 128 bytes and the ring is a flat array of them,
// so a struct of any other size puts every entry but the first at the wrong offset.
// Checked at compile time because the runtime symptom is EINVAL from an unrelated-looking
// command.
const _ = 1 / (128 / uint(unsafe.Sizeof(ioUringSQE{}))) // fails unless sizeof == 128

// ioUringCQE is struct io_uring_cqe.
type ioUringCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

// ioURing is one submission/completion ring.
type ioURing struct {
	fd int

	sqRing []byte
	cqRing []byte
	sqes   []byte

	sqTail  *uint32
	sqMask  *uint32
	sqArray []uint32

	cqHead *uint32
	cqTail *uint32
	cqMask *uint32
	cqes   unsafe.Pointer

	entries uint32
}

// newIOURing sets up a ring with the given queue depth.
//
// Depth is a power of two because the kernel requires it and because the ring indices
// wrap by masking; a non-power-of-two would be silently rounded up and the mask this
// code reads would then disagree with the one the kernel uses.
func newIOURing(entries uint32) (*ioURing, error) {
	p := ioUringParams{Flags: ioringSetupSQE128}
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP,
		uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &ioURing{fd: int(fd), entries: p.SQEntries}

	sqSize := p.SQOff.Array + p.SQEntries*4
	cqSize := p.CQOff.CQEs + p.CQEntries*uint32(unsafe.Sizeof(ioUringCQE{}))

	var err error
	// Mapped separately rather than with IORING_FEAT_SINGLE_MMAP's combined mapping,
	// because that feature is not present on every kernel this may run on and the
	// separate form works everywhere.
	r.sqRing, err = unix.Mmap(r.fd, ioringOffSQRing, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Close(r.fd)
		return nil, fmt.Errorf("mmap sq ring: %w", err)
	}
	r.cqRing, err = unix.Mmap(r.fd, ioringOffCQRing, int(cqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("mmap cq ring: %w", err)
	}
	r.sqes, err = unix.Mmap(r.fd, ioringOffSQES,
		int(p.SQEntries)*int(unsafe.Sizeof(ioUringSQE{})),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("mmap sqes: %w", err)
	}

	r.sqTail = (*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.Tail]))
	r.sqMask = (*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.RingMask]))
	r.sqArray = unsafe.Slice(
		(*uint32)(unsafe.Pointer(&r.sqRing[p.SQOff.Array])), p.SQEntries)

	r.cqHead = (*uint32)(unsafe.Pointer(&r.cqRing[p.CQOff.Head]))
	r.cqTail = (*uint32)(unsafe.Pointer(&r.cqRing[p.CQOff.Tail]))
	r.cqMask = (*uint32)(unsafe.Pointer(&r.cqRing[p.CQOff.RingMask]))
	r.cqes = unsafe.Pointer(&r.cqRing[p.CQOff.CQEs])

	return r, nil
}

func (r *ioURing) Close() error {
	for _, m := range [][]byte{r.sqes, r.cqRing, r.sqRing} {
		if m != nil {
			_ = unix.Munmap(m)
		}
	}
	r.sqes, r.cqRing, r.sqRing = nil, nil, nil
	if r.fd > 0 {
		err := unix.Close(r.fd)
		r.fd = -1
		return err
	}
	return nil
}

// sqe returns the submission entry at the ring's current tail.
func (r *ioURing) sqe() *ioUringSQE {
	tail := atomic.LoadUint32(r.sqTail)
	idx := tail & atomic.LoadUint32(r.sqMask)
	// The array maps ring slots to sqe indices. Identity mapping is what every
	// non-SQPOLL user does, and it must be written or the kernel reads slot 0 for
	// every submission.
	r.sqArray[idx] = idx
	return (*ioUringSQE)(unsafe.Pointer(
		&r.sqes[uintptr(idx)*unsafe.Sizeof(ioUringSQE{})]))
}

// submitAndWait publishes one prepared SQE and blocks for its completion.
//
// One at a time on purpose: every caller here is a control command that has to be
// acknowledged before the next step makes sense -- a device cannot be started before
// its parameters are set -- so batching would only hide ordering mistakes.
func (r *ioURing) submitAndWait() (int32, error) {
	cqe, err := r.submitAndWaitCQE()
	if err != nil {
		return 0, err
	}
	return cqe.Res, nil
}

// submitAndWaitCQE is submitAndWait returning the whole completion, so a caller can
// check which submission it belongs to.
func (r *ioURing) submitAndWaitCQE() (ioUringCQE, error) {
	// The tail store must be released after the SQE writes above are visible, which is
	// what the kernel's memory-ordering contract requires of userspace.
	atomic.AddUint32(r.sqTail, 1)

	// Looped rather than entered once. io_uring_enter can return having submitted the
	// SQE without a completion being ready -- a signal, or the kernel choosing to
	// complete the work asynchronously, both do that. Treating the first return as
	// proof of a completion produced "enter returned with no completion" on 195 of 256
	// concurrent creates, an error that describes the symptom and accuses nothing.
	//
	// Bounded rather than infinite: a command that never completes is a bug worth an
	// error, not a hang. The limit is generous because a ublk control command waits on
	// the driver, and START_DEV in particular waits for a queue to arm.
	const maxEnters = 1000
	for i := 0; i < maxEnters; i++ {
		toSubmit := uintptr(0)
		if i == 0 {
			toSubmit = 1
		}
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
			uintptr(r.fd), toSubmit, 1, ioringEnterGetEvents, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return ioUringCQE{}, fmt.Errorf("io_uring_enter: %w", errno)
		}

		head := atomic.LoadUint32(r.cqHead)
		if head != atomic.LoadUint32(r.cqTail) {
			cqe := *(*ioUringCQE)(unsafe.Pointer(uintptr(r.cqes) +
				uintptr(head&atomic.LoadUint32(r.cqMask))*unsafe.Sizeof(ioUringCQE{})))
			atomic.StoreUint32(r.cqHead, head+1)
			return cqe, nil
		}
	}
	return ioUringCQE{}, fmt.Errorf("io_uring: no completion after %d enters", maxEnters)
}

// uringCmd submits one IORING_OP_URING_CMD against fd and returns the kernel's result.
//
// payload is copied into the SQE's inline command area, which is 16 bytes. ublk's
// control commands are larger than that, so they pass a pointer in the payload instead
// -- see ublkCtrl.cmd for how that is arranged.
func (r *ioURing) uringCmd(fd int, cmdOp uint32, payload []byte) (int32, error) {
	if len(payload) > len(ioUringSQE{}.Cmd) {
		return 0, fmt.Errorf("io_uring: inline cmd payload is %d bytes, max %d",
			len(payload), len(ioUringSQE{}.Cmd))
	}
	s := r.sqe()
	*s = ioUringSQE{
		Opcode: ioringOpUringCmd,
		FD:     int32(fd),
		CmdOp:  cmdOp,
	}
	copy(s.Cmd[:], payload)
	return r.submitAndWait()
}

// submitAndWaitUser is submitAndWait with a check that the completion belongs to the
// submission.
//
// It exists because a passing nop proves less than it appears to: if the kernel reads a
// different SQE slot than the one just written, a nop there still completes successfully
// and the ring looks healthy while every command's payload is being ignored. Comparing
// UserData is what distinguishes those two.
func (r *ioURing) submitAndWaitUser(wantUserData uint64) (int32, error) {
	res, err := r.submitAndWaitCQE()
	if err != nil {
		return 0, err
	}
	if res.UserData != wantUserData {
		return 0, fmt.Errorf("io_uring: completion carries user_data %#x, submitted %#x; "+
			"the kernel is reading a different submission slot than the one written",
			res.UserData, wantUserData)
	}
	return res.Res, nil
}

// publish makes the SQE at the tail visible to the kernel without entering it.
//
// Split from submission because a queue's whole purpose is to have many requests in
// flight: submitAndWait would serialise the depth back down to one.
func (r *ioURing) publish() {
	atomic.AddUint32(r.sqTail, 1)
}

// enter submits the published SQEs and optionally waits for completions.
//
// A thin wrapper on the syscall, so a caller decides whether it wants to block. EINTR is
// returned rather than retried: the caller knows whether it is in a loop that can simply
// go round again, and swallowing it here would hide a stop signal.
func (r *ioURing) enter(toSubmit, minComplete uint32) error {
	var flags uintptr
	if minComplete > 0 {
		flags = ioringEnterGetEvents
	}
	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(toSubmit), uintptr(minComplete), flags, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// waitCQE blocks for the next completion and consumes it.
//
// Checks the ring before entering the kernel: under load the completion is usually
// already there, and a syscall to discover that is the cost this avoids on the IO path.
func (r *ioURing) waitCQE() (ioUringCQE, error) {
	for {
		head := atomic.LoadUint32(r.cqHead)
		if head != atomic.LoadUint32(r.cqTail) {
			cqe := *(*ioUringCQE)(unsafe.Pointer(uintptr(r.cqes) +
				uintptr(head&atomic.LoadUint32(r.cqMask))*unsafe.Sizeof(ioUringCQE{})))
			atomic.StoreUint32(r.cqHead, head+1)
			return cqe, nil
		}
		if err := r.enter(0, 1); err != nil {
			return ioUringCQE{}, err
		}
	}
}
