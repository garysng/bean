//go:build linux

package image

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestIOURingSQE128IsGranted separates two failures that look identical.
//
// A GET_FEATURES returning EINVAL can mean the command is malformed, or that the ring
// never got the 128-byte SQEs the command needs. The second is silent: io_uring_setup
// does not fail on an unsupported flag, it clears it and succeeds -- so the inline
// payload is truncated at 16 bytes and the kernel rejects a command that reads correctly
// in the source.
//
// Written as its own test because it must be able to pass while the GET_FEATURES probe
// fails. That combination is the useful signal: it says the ring is fine and the fault
// is in the command.
func TestIOURingSQE128IsGranted(t *testing.T) {
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("no ublk on this host (%v)", err)
	}
	p := ioUringParams{Flags: ioringSetupSQE128}
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 8,
		uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		t.Fatalf("io_uring_setup: %v", errno)
	}
	defer unix.Close(int(fd))

	if p.Flags&ioringSetupSQE128 == 0 {
		t.Fatal("the kernel cleared IORING_SETUP_SQE128, so the inline command area is " +
			"16 bytes and no ublk control command can fit")
	}
	t.Logf("SQE128 granted; flags=%#x features=%#x sqEntries=%d cqEntries=%d",
		p.Flags, p.Features, p.SQEntries, p.CQEntries)
	t.Logf("sizeof(ioUringSQE)=%d sizeof(ublksrvCtrlCmd)=%d",
		unsafe.Sizeof(ioUringSQE{}), unsafe.Sizeof(ublksrvCtrlCmd{}))
}
