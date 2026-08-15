//go:build linux

package image

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestUblkGetFeaturesOnHardware is the first thing that proves the io_uring layer works.
//
// GET_FEATURES is chosen deliberately as the first command to attempt: it takes no
// arguments, creates nothing, and cannot leave a device behind. If the ring, the SQE
// layout, the command encoding and the completion read are all correct, this returns a
// non-zero feature mask; if any one of them is wrong, it returns an errno and nothing
// has been changed on the host.
//
// Skipped without /dev/ublk-control so the suite still runs on a kernel below 6.0 or in
// a container -- and skipped loudly, because a silent pass would mean this file proves
// nothing on the machine where it matters.
func TestUblkGetFeaturesOnHardware(t *testing.T) {
	f, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ublk-control (%v); needs kernel >= 6.0 with ublk_drv loaded", err)
	}
	defer f.Close()

	ring, err := newIOURing(8)
	if err != nil {
		t.Fatalf("io_uring setup failed, so nothing above it can be trusted: %v", err)
	}
	defer ring.Close()

	// The kernel writes the feature mask to the address in the command's Addr field.
	var features uint64
	cmd := ublksrvCtrlCmd{
		Addr: uint64(uintptr(unsafe.Pointer(&features))),
		Len:  uint16(unsafe.Sizeof(features)),
		// 0xffff, not 0. A zero queue_id names queue 0, which for a command that is
		// not about a queue makes the kernel validate against a device that does not
		// exist -- the EINVAL this test first returned. agentenv sets u16::MAX here for
		// the same reason.
		QueueID: 0xffff,
	}
	// The struct is copied into the SQE's inline area, which IORING_SETUP_SQE128 makes
	// 80 bytes -- enough for all 32 of it.
	payload := (*[unsafe.Sizeof(ublksrvCtrlCmd{})]byte)(unsafe.Pointer(&cmd))[:]

	res, err := ring.uringCmd(int(f.Fd()), ublkCmdGetFeatures, payload)
	if err != nil {
		t.Fatalf("submitting GET_FEATURES: %v", err)
	}
	if res < 0 {
		t.Fatalf("GET_FEATURES returned %d (%v). A negative result here is the kernel "+
			"rejecting the command, which points at the encoding or the SQE layout "+
			"rather than at ublk being unavailable", res, unix.Errno(-res))
	}
	if features == 0 {
		t.Fatal("GET_FEATURES succeeded but the mask is zero, so the kernel wrote " +
			"nowhere this process can see -- the Addr field is not being honoured")
	}
	t.Logf("ublk features = %#x (USER_COPY=%v UPDATE_SIZE=%v)",
		features, features&ublkFUserCopy != 0, features&ublkFUpdateSize != 0)
}
