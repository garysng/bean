//go:build linux

package image

import "testing"

// TestUblkCommandEncoding checks the two command words the kernel documents.
//
// The encoding multiplies the struct size into the command number, so a field added to
// ublksrvCtrlCmd in the wrong place produces a different command word and the kernel
// rejects it as unknown -- an EINVAL that names nothing. Anchoring on a value from the
// uapi header is what turns that into a compile-time-ish failure here instead.
func TestUblkCommandEncoding(t *testing.T) {
	// From include/uapi/linux/ublk_cmd.h:
	//   #define UBLK_U_CMD_UPDATE_SIZE _IOWR('u', 0x15, struct ublksrv_ctrl_cmd)
	// agentenv's ublk_caps.rs asserts the same literal, independently derived.
	if got := ublkCmdUpdateSize; got != 0xC0207515 {
		t.Errorf("UBLK_U_CMD_UPDATE_SIZE = %#x, want 0xC0207515.\n"+
			"The size term comes from unsafe.Sizeof(ublksrvCtrlCmd{}), so this failing "+
			"means the struct does not match the kernel's 32-byte layout", got)
	}
	if ublkFUpdateSize != 1<<10 {
		t.Errorf("UBLK_F_UPDATE_SIZE = %#x, want 1<<10", ublkFUpdateSize)
	}
}
