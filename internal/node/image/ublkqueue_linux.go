//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A ublk queue: the loop that serves one device's block IO from userspace.
//
// The protocol is a credit system rather than a request/response. For each of the
// queue's slots the server submits a FETCH_REQ, which completes when the kernel has a
// request for that slot. The server reads the request from a shared mmap, does the work,
// and submits COMMIT_AND_FETCH_REQ -- one command that both reports the result and asks
// for the next request. So a slot is always either held by the kernel or by the server,
// and the queue depth is the number of requests that can be in flight.
//
// Data moves by pread/pwrite on the char device rather than by mapping the kernel's
// buffers, which is what UBLK_F_USER_COPY means. The offset encodes the queue, the tag
// and the byte offset within the request, so one file handle serves every slot.

const (
	// ublkMaxQueueDepth is UBLK_MAX_QUEUE_DEPTH. It sizes the per-queue descriptor
	// region the kernel reserves, so it is part of the mmap offset arithmetic even for
	// a queue that is shallower than this.
	ublkMaxQueueDepth = 4096

	// ublksrvCmdBufOffset and ublksrvIOBufOffset are the mmap and pread/pwrite offsets
	// the driver publishes. See UBLKSRV_CMD_BUF_OFFSET and UBLKSRV_IO_BUF_OFFSET.
	ublksrvCmdBufOffset = 0
	ublksrvIOBufOffset  = 0x80000000

	// The offset encoding for USER_COPY data transfers, from ublk_cmd.h: the tag and
	// queue id are packed above the per-request byte offset.
	ublkIOBufBits = 25
	ublkTagOff    = ublkIOBufBits
	ublkTagBits   = 16
	ublkQIDOff    = ublkTagOff + ublkTagBits
)

// ublkBackend serves one device's reads and writes.
//
// An interface rather than the overlaybd reader directly, so the queue can be tested
// against something with no kernel dependencies at all -- the protocol is what is hard
// here, and a bug in it would otherwise only be reachable through a real device.
type ublkBackend interface {
	// ReadAt and WriteAt follow io.ReaderAt/WriterAt semantics.
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	// Flush makes previous writes durable. Called for the kernel's flush requests,
	// which a filesystem issues to order its journal.
	Flush() error
}

// ublkQueue serves one queue of one device.
type ublkQueue struct {
	qid     uint16
	depth   uint16
	cdev    *os.File
	ring    *ioURing
	backend ublkBackend

	// descs is the kernel's request array, mapped read-only. The kernel writes a
	// request here and the completion tells the server which slot to read.
	descs []byte
	// bufs holds one staging buffer per slot. Allocated once: a per-request allocation
	// on the IO path would put the garbage collector between the guest and its disk.
	bufs [][]byte

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	err      error
}

// newUblkQueue maps the queue's descriptor region and prepares its buffers.
//
// maxIOBytes bounds one request and therefore each staging buffer. It must match what
// ADD_DEV was told, because the kernel will not send a request larger than that and a
// smaller buffer here would silently truncate one that is.
func newUblkQueue(cdev *os.File, qid, depth uint16, maxIOBytes uint32, backend ublkBackend) (*ublkQueue, error) {
	if backend == nil {
		return nil, errors.New("image: ublk queue needs a backend")
	}
	descSize := int(unsafe.Sizeof(ublkIODesc{}))

	// The kernel reserves a full UBLK_MAX_QUEUE_DEPTH worth of descriptors per queue,
	// rounded up to a page, whatever this queue's depth is. Using the actual depth here
	// would put queue 1's mapping inside queue 0's region.
	pageSize := os.Getpagesize()
	maxCmdBufSize := ublkMaxQueueDepth * descSize
	if r := maxCmdBufSize % pageSize; r != 0 {
		maxCmdBufSize += pageSize - r
	}
	off := int64(ublksrvCmdBufOffset) + int64(qid)*int64(maxCmdBufSize)

	// Mapped read-only: the descriptors are the kernel's to write. A writable mapping
	// would let a bug here corrupt the request the guest is waiting on.
	descs, err := unix.Mmap(int(cdev.Fd()), off, int(depth)*descSize,
		unix.PROT_READ, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("image: mmap ublk descriptors (qid=%d off=%#x): %w",
			qid, off, err)
	}

	// Depth+1 so a completion and its follow-up submission never contend for the last
	// slot. The ring must be at least as deep as the queue or a COMMIT_AND_FETCH would
	// have nowhere to go.
	ring, err := newIOURing(nextPow2(uint32(depth) + 1))
	if err != nil {
		_ = unix.Munmap(descs)
		return nil, fmt.Errorf("image: io_uring for ublk queue: %w", err)
	}

	q := &ublkQueue{
		qid:     qid,
		depth:   depth,
		cdev:    cdev,
		ring:    ring,
		backend: backend,
		descs:   descs,
		bufs:    make([][]byte, depth),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for i := range q.bufs {
		q.bufs[i] = make([]byte, maxIOBytes)
	}
	return q, nil
}

func nextPow2(n uint32) uint32 {
	p := uint32(1)
	for p < n {
		p <<= 1
	}
	return p
}

// desc reads the kernel's request for a slot.
func (q *ublkQueue) desc(tag uint16) ublkIODesc {
	sz := unsafe.Sizeof(ublkIODesc{})
	return *(*ublkIODesc)(unsafe.Pointer(&q.descs[uintptr(tag)*sz]))
}

// ioOffset is the pread/pwrite offset for a slot's data under USER_COPY.
func (q *ublkQueue) ioOffset(tag uint16) int64 {
	return int64(ublksrvIOBufOffset) |
		(int64(q.qid) << ublkQIDOff) |
		(int64(tag) << ublkTagOff)
}

// submitIOCmd sends one FETCH_REQ or COMMIT_AND_FETCH_REQ for a slot.
func (q *ublkQueue) submitIOCmd(op uint32, tag uint16, result int32) error {
	cmd := ublkIOCmd{QID: q.qid, Tag: tag, Result: result}
	payload := (*[unsafe.Sizeof(ublkIOCmd{})]byte)(unsafe.Pointer(&cmd))[:]

	s := q.ring.sqe()
	*s = ioUringSQE{
		Opcode:   ioringOpUringCmd,
		FD:       int32(q.cdev.Fd()),
		CmdOp:    op,
		UserData: uint64(tag),
	}
	copy(s.Cmd[:], payload)
	// Published without waiting: the whole point of the queue is that submissions and
	// completions are decoupled, and waiting here would serialise the depth away.
	q.ring.publish()
	return nil
}

// Start runs the serve loop and waits until every slot is armed.
//
// The priming happens on the serve goroutine rather than here, and that is the whole
// design constraint of this file. The kernel records the thread that arms the last slot
// as the queue's daemon -- `ubq->ubq_daemon = current` in ublk_mark_io_ready -- and then
// rejects any later command from a different one: `if (ubq->ubq_daemon &&
// ubq->ubq_daemon != current) goto out`. Go moves goroutines between OS threads freely,
// so arming from one goroutine and serving from another binds the queue to a thread that
// may never issue another command, and every subsequent fetch is rejected.
//
// Measured before the cause was known: START_DEV blocked forever in the kernel, because
// it waits for nr_io_ready == q_depth and the fetches were being refused. The stack said
// blk_add_partitions -> read_part_sector, which is the partition scan the new disk
// triggers -- a read with nobody to answer it. The process was left unkillable in D
// state and the host needed a reboot.
//
// Waiting for the arm to finish is also required rather than tidy: the caller issues
// START_DEV next, and START_DEV is what blocks if the slots are not ready.
func (q *ublkQueue) Start() error {
	armed := make(chan error, 1)
	go q.serve(armed)
	return <-armed
}

// Stop ends the serve loop and releases the queue's mappings.
func (q *ublkQueue) Stop() error {
	q.stopOnce.Do(func() { close(q.stop) })
	<-q.done

	var errs []error
	if q.descs != nil {
		errs = append(errs, unix.Munmap(q.descs))
		q.descs = nil
	}
	if q.ring != nil {
		errs = append(errs, q.ring.Close())
		q.ring = nil
	}
	if q.err != nil {
		errs = append(errs, q.err)
	}
	return errors.Join(errs...)
}

// serve arms the queue and then handles requests until Stop.
//
// runtime.LockOSThread is load-bearing, not defensive: see Start for the kernel's
// ubq_daemon check. Every uring_cmd for this queue -- the initial fetches and every
// commit-and-fetch after -- must issue from this one thread for the queue's lifetime.
func (q *ublkQueue) serve(armed chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(q.done)

	for tag := uint16(0); tag < q.depth; tag++ {
		if err := q.submitIOCmd(ublkIOFetchReq, tag, 0); err != nil {
			armed <- fmt.Errorf("image: prime slot %d: %w", tag, err)
			return
		}
	}
	// Submitted without waiting for completions: a fetch completes when the kernel has
	// a request for that slot, which for a device that has not started yet is never. The
	// arm is done when the kernel has accepted the submissions, not when they complete.
	if err := q.ring.enter(uint32(q.depth), 0); err != nil {
		armed <- fmt.Errorf("image: submit initial fetches: %w", err)
		return
	}
	armed <- nil

	for {
		select {
		case <-q.stop:
			return
		default:
		}

		cqe, err := q.ring.waitCQE()
		if err != nil {
			// EINTR is ordinary: Go's runtime signals threads, and a wait interrupted
			// by that has nothing wrong with it.
			if errors.Is(err, unix.EINTR) {
				continue
			}
			select {
			case <-q.stop:
				return
			default:
			}
			q.err = fmt.Errorf("image: ublk queue %d wait: %w", q.qid, err)
			return
		}

		tag := uint16(cqe.UserData)
		if cqe.Res < 0 {
			// UBLK_IO_RES_ABORT (-ENODEV) is how the kernel says the device is going
			// away, which happens on every normal stop -- so it ends the loop quietly
			// rather than being reported as a failure.
			if unix.Errno(-cqe.Res) == unix.ENODEV {
				return
			}
			q.err = fmt.Errorf("image: ublk queue %d slot %d: %w",
				q.qid, tag, unix.Errno(-cqe.Res))
			return
		}

		res := q.handle(tag)
		if err := q.submitIOCmd(ublkIOCommitAndFetch, tag, res); err != nil {
			q.err = err
			return
		}
		if err := q.ring.enter(1, 0); err != nil {
			q.err = fmt.Errorf("image: ublk queue %d commit: %w", q.qid, err)
			return
		}
	}
}

// handle performs one request and returns the result the kernel expects: the number of
// bytes transferred, or a negative errno.
func (q *ublkQueue) handle(tag uint16) int32 {
	if int(tag) >= len(q.bufs) {
		return -int32(unix.EINVAL)
	}
	d := q.desc(tag)
	length := int(d.NRSectors) << 9
	offset := int64(d.StartSector) << 9
	buf := q.bufs[tag]
	if length > len(buf) {
		// The kernel was told a maximum at ADD_DEV, so this cannot happen unless the
		// two disagree -- and truncating would corrupt the guest's data silently.
		return -int32(unix.EINVAL)
	}

	switch d.OpFlags & ublkIOOpMask {
	case ublkIOOpRead:
		n, err := q.backend.ReadAt(buf[:length], offset)
		if err != nil && n < length {
			return -int32(unix.EIO)
		}
		// Written to the driver's buffer through the char device: that is what
		// USER_COPY means, and the offset names the slot.
		if _, err := q.cdev.WriteAt(buf[:length], q.ioOffset(tag)); err != nil {
			return -int32(unix.EIO)
		}
		return int32(length)

	case ublkIOOpWrite:
		if _, err := q.cdev.ReadAt(buf[:length], q.ioOffset(tag)); err != nil {
			return -int32(unix.EIO)
		}
		if _, err := q.backend.WriteAt(buf[:length], offset); err != nil {
			return -int32(unix.EIO)
		}
		return int32(length)

	case ublkIOOpFlush:
		if err := q.backend.Flush(); err != nil {
			return -int32(unix.EIO)
		}
		return 0

	case ublkIOOpDiscard, ublkIOOpWriteZero:
		// Accepted and not implemented. Reporting success is correct for discard --
		// it is advisory -- but write-zeroes is not, so it is refused instead of
		// silently doing nothing to blocks the guest believes are now zero.
		if d.OpFlags&ublkIOOpMask == ublkIOOpWriteZero {
			return -int32(unix.EOPNOTSUPP)
		}
		return 0

	default:
		return -int32(unix.EOPNOTSUPP)
	}
}
