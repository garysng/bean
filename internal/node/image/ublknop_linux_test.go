//go:build linux

package image

import (
	"testing"
	"unsafe"
)

// TestIOURingNopCompletes checks the ring itself with a command that cannot fail.
//
// IORING_OP_NOP takes no arguments and always succeeds, so a non-zero result here means
// the fault is in the ring -- the tail store, the sqArray mapping, or the completion
// read -- and not in any ublk command. Without this, a ublk EINVAL is ambiguous between
// "wrong command" and "the kernel never saw the command I wrote".
func TestIOURingNopCompletes(t *testing.T) {
	r, err := newIOURing(8)
	if err != nil {
		t.Skipf("io_uring unavailable (%v)", err)
	}
	defer r.Close()

	const ioringOpNop = 0
	s := r.sqe()
	*s = ioUringSQE{Opcode: ioringOpNop, UserData: 0x5eed}
	// Recorded before submitting: if the kernel reads a different slot than the one
	// written, the completion carries a zero UserData and the nop still succeeds -- so
	// a passing nop alone does not prove the right SQE was read.
	wantUser := s.UserData
	res, err := r.submitAndWaitUser(wantUser)
	if err != nil {
		t.Fatalf("submitting a nop failed, so the ring is wrong: %v", err)
	}
	if res != 0 {
		t.Fatalf("nop returned %d, want 0; the ring's submit or completion path is wrong",
			res)
	}
	t.Logf("ring works: nop completed, sizeof(sqe)=%d", unsafe.Sizeof(ioUringSQE{}))
}
