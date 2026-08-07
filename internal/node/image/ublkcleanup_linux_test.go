//go:build linux

package image

import (
	"fmt"
	"os"
	"testing"
)

// TestUblkReclaimOrphans removes ublk devices left behind by an interrupted server.
//
// Not a test of behaviour: it is the reclaim path, written as a test so it can be run
// against a host without shipping a separate binary. It exists because a ublk device
// whose server dies does not go away -- the kernel keeps the disk and waits -- so an
// interrupted run leaves /dev/ublkb<N> holding a minor number and a kernel thread.
//
// Guarded by an environment variable so it cannot run by accident: on a shared host it
// would delete devices belonging to something else.
func TestUblkReclaimOrphans(t *testing.T) {
	if os.Getenv("BEAN_UBLK_RECLAIM") == "" {
		t.Skip("set BEAN_UBLK_RECLAIM=1 to delete every ublk device on this host")
	}
	c, err := openUblkControl()
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer c.Close()

	for id := uint32(0); id < 64; id++ {
		if _, err := os.Stat(fmt.Sprintf("/dev/ublkc%d", id)); err != nil {
			continue
		}
		// Stop before delete: a device whose disk is still visible cannot be removed,
		// and the stop is what detaches it from the block layer.
		if err := c.stopDevice(id); err != nil {
			t.Logf("stop %d: %v", id, err)
		}
		if err := c.deleteDevice(id); err != nil {
			t.Errorf("delete %d: %v", id, err)
			continue
		}
		t.Logf("reclaimed ublk device %d", id)
	}
}
