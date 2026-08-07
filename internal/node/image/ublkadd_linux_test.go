//go:build linux

package image

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ublksrvCtrlDevInfo is struct ublksrv_ctrl_dev_info, the payload ADD_DEV takes.
type ublksrvCtrlDevInfo struct {
	NRHWQueues    uint16
	QueueDepth    uint16
	State         uint16
	Pad0          uint16
	MaxIOBufBytes uint32
	DevID         uint32
	UBLKSRVPID    int32
	Pad1          uint32
	Flags         uint64
	TSData        [1]uint64
	OwnerUID      uint32
	OwnerGID      uint32
	Reserved1     uint64
	Reserved2     uint64
}

// TestUblkAddDevOnHardware tries the command that actually matters.
//
// GET_FEATURES returns EINVAL to every variation -- both ioctl directions, every field
// varied, a pinned buffer instead of a Go pointer -- while the ring is proven correct by a
// nop whose user_data round-trips and an SQE verified at 128 bytes with cmd at offset 48.
// That combination suggests GET_FEATURES may not be wired on this kernel build; it is the
// newest of these commands. ADD_DEV is the one bean needs and the one every ublk server
// issues first, so it is the better probe.
//
// The device is deleted before returning. A leaked ublk device holds a minor number and a
// kernel thread, and this runs on a host with other people's workloads.
func TestUblkAddDevOnHardware(t *testing.T) {
	f, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no ublk control device (%v)", err)
	}
	defer f.Close()

	r, err := newIOURing(8)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	defer r.Close()

	info := ublksrvCtrlDevInfo{
		NRHWQueues:    1,
		QueueDepth:    16,
		MaxIOBufBytes: 256 << 10,
		DevID:         0xffffffff, // let the kernel choose
		Flags:         ublkFUserCopy,
	}
	cmd := ublksrvCtrlCmd{
		DevID:   0xffffffff,
		QueueID: 0xffff,
		Len:     uint16(unsafe.Sizeof(info)),
		Addr:    uint64(uintptr(unsafe.Pointer(&info))),
	}
	payload := (*[unsafe.Sizeof(ublksrvCtrlCmd{})]byte)(unsafe.Pointer(&cmd))[:]

	res, err := r.uringCmd(int(f.Fd()), ublkCmdAddDev, payload)
	if err != nil {
		t.Fatalf("submit ADD_DEV: %v", err)
	}
	if res < 0 {
		t.Fatalf("ADD_DEV returned %d (%v); sizeof(devInfo)=%d, sizeof(ctrlCmd)=%d",
			res, unix.Errno(-res), unsafe.Sizeof(info), unsafe.Sizeof(cmd))
	}

	t.Logf("ADD_DEV succeeded: dev_id=%d", info.DevID)

	del := ublksrvCtrlCmd{DevID: info.DevID, QueueID: 0xffff}
	delPayload := (*[unsafe.Sizeof(ublksrvCtrlCmd{})]byte)(unsafe.Pointer(&del))[:]
	if dres, derr := r.uringCmd(int(f.Fd()), ublkCmdDelDev, delPayload); derr != nil || dres < 0 {
		t.Errorf("DEL_DEV failed (res=%d err=%v): device %d is leaked on this host",
			dres, derr, info.DevID)
	}
}
