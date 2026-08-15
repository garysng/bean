//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// attachUblk presents a backend as a block device and returns it ready for use.
//
// The sequence and its reasons: add the device so the kernel allocates an id and creates
// the char device; open that char device, because the queue's descriptor mapping and its
// data transfers both go through it; start the queue so fetches are outstanding; set the
// parameters, which START_DEV will read; then start the device, which creates the disk.
//
// Starting the queue before the device is not an optimisation. START_DEV makes the disk
// visible and the kernel may deliver a request immediately -- a partition scan does
// exactly that -- and a request arriving with no slot waiting stalls until one appears.
func attachUblk(ctrl *ublkControl, backend ublkBackend, sizeBytes int64) (dev *ublkDevice, err error) {
	const (
		queues     = 1
		depth      = 32
		maxIOBytes = 256 << 10
	)

	features, err := ctrl.Features()
	if err != nil {
		return nil, err
	}
	// Checked rather than assumed, because the alternative to USER_COPY is mapping the
	// driver's buffers, which this implementation does not do -- and without the check
	// the failure would be a device whose reads return garbage.
	if features&ublkFUserCopy == 0 {
		return nil, fmt.Errorf("image: this kernel's ublk lacks UBLK_F_USER_COPY "+
			"(features=%#x), which this server requires", features)
	}

	info, err := ctrl.addDevice(queues, depth, maxIOBytes, ublkFUserCopy)
	if err != nil {
		return nil, err
	}
	devID := info.DevID

	// Unwound in reverse on any failure below. Registered as a closure list rather than
	// a defer per step so the order is visible in one place: a half-attached device
	// holds a minor number and a kernel thread.
	var undo []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}()
	undo = append(undo, func() { _ = ctrl.deleteDevice(devID) })

	charPath := fmt.Sprintf("/dev/ublkc%d", devID)
	cdev, err := waitAndOpen(charPath, 3*time.Second)
	if err != nil {
		return nil, err
	}
	undo = append(undo, func() { _ = cdev.Close() })

	q, err := newUblkQueue(cdev, 0, depth, maxIOBytes, backend)
	if err != nil {
		return nil, err
	}
	undo = append(undo, func() { _ = q.Stop() })

	if err := q.Start(); err != nil {
		return nil, err
	}

	if err := ctrl.setParams(devID, sizeBytes); err != nil {
		return nil, err
	}
	// The kernel records the serving process and requires it to be alive, so this is
	// the pid of whoever is running the queue -- this process.
	if err := ctrl.startDevice(devID, os.Getpid()); err != nil {
		return nil, err
	}
	undo = append(undo, func() { _ = ctrl.stopDevice(devID) })

	blockPath := fmt.Sprintf("/dev/ublkb%d", devID)
	if err := waitForPath(blockPath, 3*time.Second); err != nil {
		return nil, err
	}

	return &ublkDevice{
		DevID:  devID,
		Device: blockPath,
		Size:   sizeBytes,
		ctrl:   ctrl,
		cdev:   cdev,
		queue:  q,
	}, nil
}

// detach removes the device and stops serving it.
//
// Ordered stop-then-delete, and the queue is stopped between them: a queue still fetching
// from a deleted device is a use of a freed kernel object, and deleting while the disk is
// still visible is a disk whose next read has nobody to answer it.
func (d *ublkDevice) detach() error {
	var errs []error
	// A failing stop does not skip the delete. STOP_DEV waits on the queue, so a queue
	// that has already died makes it fail or hang -- and returning here would leave the
	// device allocated forever, which is measurable: 143 devices accumulated on a host
	// whose ublks_max is 64, from creates that failed after ADD_DEV.
	if err := d.ctrl.stopDevice(d.DevID); err != nil {
		errs = append(errs, fmt.Errorf("stop ublk device %d: %w", d.DevID, err))
	}
	if d.queue != nil {
		if err := d.queue.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop ublk queue: %w", err))
		}
		d.queue = nil
	}
	if d.cdev != nil {
		if err := d.cdev.Close(); err != nil {
			errs = append(errs, err)
		}
		d.cdev = nil
	}
	if err := d.ctrl.deleteDevice(d.DevID); err != nil {
		errs = append(errs, fmt.Errorf("delete ublk device %d: %w", d.DevID, err))
	}
	return errors.Join(errs...)
}

// waitAndOpen opens a device node once it appears.
//
// Polled because the node is created by udev reacting to the kernel, which is
// asynchronous with the control command returning -- the same reason the tcmu path polls
// for its block device.
func waitAndOpen(path string, timeout time.Duration) (*os.File, error) {
	if err := waitForPath(path, timeout); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("image: open %s: %w", path, err)
	}
	return f, nil
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("image: %s did not appear within %s; the ublk device was "+
				"created but its node was not, which is a udev problem rather than a "+
				"driver one", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
