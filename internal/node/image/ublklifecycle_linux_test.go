//go:build linux

package image

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestUblkLifecycleOnHardware drives add, setParams and delete against the kernel.
//
// START_DEV is deliberately not called yet: it creates the gendisk and the kernel then
// expects a server fetching IO on the char device, so starting without one produces a
// block device whose first read hangs. That half needs the queue, which is the next piece.
//
// What this does establish is the whole control sequence up to that point, including the
// ordering that has no error to report when violated: parameters must be set before the
// device starts, so if setParams fails here it fails loudly instead of producing a
// zero-sized disk later.
//
// Everything is torn down before returning, and the check for a leak is part of the test
// rather than left to the operator: a leaked ublk device holds a minor number and a
// kernel thread on a host running other people's workloads.
func TestUblkLifecycleOnHardware(t *testing.T) {
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("no ublk on this host (%v)", err)
	}

	c, err := openUblkControl()
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer c.Close()

	features, err := c.Features()
	if err != nil {
		t.Fatalf("GET_FEATURES: %v", err)
	}
	if features&ublkFUserCopy == 0 {
		t.Fatalf("this kernel's ublk lacks UBLK_F_USER_COPY (features=%#x); without it "+
			"a userspace server has to map the driver's buffers, which this "+
			"implementation does not do", features)
	}
	t.Logf("features=%#x USER_COPY=true", features)

	const sizeBytes = 512 << 20
	info, err := c.addDevice(1, 16, 256<<10, ublkFUserCopy)
	if err != nil {
		t.Fatalf("ADD_DEV: %v", err)
	}
	devID := info.DevID
	t.Logf("added dev_id=%d state=%d", devID, info.State)

	// Deleted no matter what happens below. Registered immediately after the add so a
	// failure in between cannot leak the device.
	defer func() {
		if err := c.deleteDevice(devID); err != nil {
			t.Errorf("DEL_DEV failed: ublk device %d is leaked on this host: %v",
				devID, err)
			return
		}
		// Checked rather than assumed: the char device disappearing is the observable
		// consequence of the delete, and a delete that returns success while the device
		// remains is the failure this catches.
		path := fmt.Sprintf("/dev/ublkc%d", devID)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s still exists after DEL_DEV returned success", path)
		}
	}()

	if err := c.setParams(devID, sizeBytes); err != nil {
		t.Fatalf("SET_PARAMS: %v", err)
	}
	t.Logf("params set: %d bytes (%d sectors)", sizeBytes, sizeBytes>>9)

	// The char device is what a server would bind. Its presence is the evidence that
	// ADD_DEV produced something real rather than only bookkeeping.
	charPath := fmt.Sprintf("/dev/ublkc%d", devID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(charPath); err == nil {
			t.Logf("%s exists", charPath)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared after ADD_DEV; the device was allocated but "+
				"the driver did not create its char device", charPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
