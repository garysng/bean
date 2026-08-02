//go:build linux

package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A restore reads the guest's memory image on demand instead of writing it to
// disk first.
//
// Firecracker's File memory backend makes a restore copy the whole image out of
// the bundle before the VM starts: for a 512 MiB guest that was 1.3s of the
// 1.4s a restore took, and it scales with guest size rather than with how much
// memory the guest actually touches. With userfaultfd, Firecracker maps the
// guest's memory anonymously and asks this process for pages as the guest faults
// them in, so a restore writes nothing and pays only for what is used.
//
// The protocol is one-shot. Firecracker connects to the socket, sends the
// userfault fd over SCM_RIGHTS with the region layout as the message body, and
// then never uses the socket again; everything after that is fault events on the
// fd itself.

// uffdMapping is one guest memory region, as Firecracker describes it during the
// handshake.
type uffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	// Offset is where this region starts in the memory image. It is not a guest
	// physical address: MMIO holes between regions are collapsed, so offsets
	// are contiguous even where guest addresses are not.
	Offset   uint64 `json:"offset"`
	PageSize uint64 `json:"page_size"`
}

// uffdHandler serves guest page faults for one microVM from a memory image.
type uffdHandler struct {
	listener *net.UnixListener
	image    []byte // mmapped memory image
	imageF   *os.File

	mu       sync.Mutex
	uffd     int
	mappings []uffdMapping
	closed   bool
	// removed tracks pages the balloon driver gave back. They must be answered
	// with zeroes rather than image contents.
	removed map[uint64]struct{}

	// failed reports why the handler stopped. Firecracker blocks forever on a
	// fault nobody answers, so a dead handler has to be visible as more than a
	// hang.
	failed chan error
	// faults counts pages served, which is how a test tells "the guest never
	// faulted" apart from "the handler never answered".
	faults atomic.Int64
}

// Faults reports how many pages have been served.
func (h *uffdHandler) Faults() int64 { return h.faults.Load() }

// newUffdHandler binds the socket and mmaps the memory image, then serves faults
// in the background.
//
// The socket must exist before the snapshot is loaded: Firecracker connects to
// it during the load request, so binding afterwards would race the VM's first
// fault.
func newUffdHandler(udsPath, memImagePath string) (*uffdHandler, error) {
	f, err := os.Open(memImagePath)
	if err != nil {
		return nil, fmt.Errorf("uffd: open memory image: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("uffd: stat memory image: %w", err)
	}
	if st.Size() == 0 {
		f.Close()
		return nil, errors.New("uffd: memory image is empty")
	}
	// Mapped read-only and shared: the image is never modified, and sharing it
	// means several VMs restored from one snapshot use one page cache copy
	// rather than one per VM.
	image, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()),
		unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("uffd: mmap memory image: %w", err)
	}

	lis, err := net.ListenUnix("unix", &net.UnixAddr{Name: udsPath, Net: "unix"})
	if err != nil {
		unix.Munmap(image)
		f.Close()
		return nil, fmt.Errorf("uffd: listen %s: %w", udsPath, err)
	}

	h := &uffdHandler{
		listener: lis,
		image:    image,
		imageF:   f,
		uffd:     -1,
		failed:   make(chan error, 1),
	}
	go h.run()
	return h, nil
}

// run waits for Firecracker, takes the userfault fd, and serves faults until the
// handler is closed.
func (h *uffdHandler) run() {
	conn, err := h.listener.AcceptUnix()
	if err != nil {
		h.fail(fmt.Errorf("uffd: accept: %w", err))
		return
	}
	defer conn.Close()
	// The socket carries only the handshake, so it is closed as soon as the fd
	// and mappings arrive.
	h.listener.Close()

	fd, mappings, err := recvUffd(conn)
	if err != nil {
		h.fail(err)
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		unix.Close(fd)
		return
	}
	h.uffd, h.mappings = fd, mappings
	h.mu.Unlock()

	h.serve()
}

// recvUffd performs the handshake: one message whose body is the region layout
// and whose control data carries the userfault fd.
func recvUffd(conn *net.UnixConn) (int, []uffdMapping, error) {
	// The descriptor and the region layout do not have to arrive in the same
	// datagram, so both are collected before either is used. Treating the first
	// read as complete leaves the handler with a descriptor it cannot use and
	// Firecracker blocked on a fault nobody will answer.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return -1, nil, fmt.Errorf("uffd: set handshake deadline: %w", err)
	}

	fd := -1
	var body []byte
	for fd < 0 || len(body) == 0 {
		buf := make([]byte, 16<<10)
		oob := make([]byte, unix.CmsgSpace(8))
		n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
		if err != nil {
			if fd >= 0 {
				unix.Close(fd)
			}
			return -1, nil, fmt.Errorf("uffd: read handshake (fd=%d, %d body bytes so far): %w",
				fd, len(body), err)
		}
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if oobn == 0 {
			continue
		}
		scms, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			if fd >= 0 {
				unix.Close(fd)
			}
			return -1, nil, fmt.Errorf("uffd: parse control message: %w", err)
		}
		for _, scm := range scms {
			fds, perr := unix.ParseUnixRights(&scm)
			if perr != nil {
				continue
			}
			for _, got := range fds {
				// Newer Firecracker sends the userfault fd first and a memfd
				// after it. Only the first is used, and any others have to be
				// closed or they leak for the life of the VM.
				if fd < 0 {
					fd = got
					continue
				}
				unix.Close(got)
			}
		}
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		unix.Close(fd)
		return -1, nil, fmt.Errorf("uffd: clear handshake deadline: %w", err)
	}

	var mappings []uffdMapping
	if err := json.Unmarshal(body, &mappings); err != nil {
		unix.Close(fd)
		return -1, nil, fmt.Errorf("uffd: decode mappings %q: %w", body, err)
	}
	if len(mappings) == 0 {
		unix.Close(fd)
		return -1, nil, errors.New("uffd: handshake carried no memory regions")
	}
	return fd, mappings, nil
}

// uffdMsg mirrors struct uffd_msg: an event byte, padding, then a union of
// per-event arguments. The layout is verified against linux/userfaultfd.h by
// TestUffdMsgMatchesKernelLayout, since reading it wrongly would silently
// misinterpret every fault address.
type uffdMsg struct {
	Event uint8
	_     [7]byte
	Arg   [24]byte
}

const (
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x15

	// Offsets into uffdMsg.Arg. A pagefault carries flags then the address; a
	// remove carries a start and an end.
	uffdPagefaultAddrOff = 8
	uffdRemoveStartOff   = 0
	uffdRemoveEndOff     = 8
)

// serve reads fault events and fills the faulting pages.
func (h *uffdHandler) serve() {
	for {
		msg, err := h.readMsg()
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, unix.EBADF) {
				return
			}
			h.fail(err)
			return
		}
		switch msg.Event {
		case uffdEventPagefault:
			addr := *(*uint64)(unsafe.Pointer(&msg.Arg[uffdPagefaultAddrOff]))
			if err := h.fill(addr); err != nil {
				h.fail(err)
				return
			}
		case uffdEventRemove:
			// The balloon driver returning memory madvises it away. The region
			// stays registered, so a later fault there must be answered with
			// zeroes: reading the image again would resurrect data the guest
			// has already discarded.
			start := *(*uint64)(unsafe.Pointer(&msg.Arg[uffdRemoveStartOff]))
			end := *(*uint64)(unsafe.Pointer(&msg.Arg[uffdRemoveEndOff]))
			h.markRemoved(start, end)
		}
	}
}

// uffdioCopy mirrors struct uffdio_copy.
type uffdioCopy struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// uffdioZeropage mirrors struct uffdio_zeropage.
type uffdioZeropage struct {
	RangeStart uint64
	RangeLen   uint64
	Mode       uint64
	Zeropage   int64
}

// Request numbers for the userfaultfd ioctls, from linux/userfaultfd.h.
const (
	uffdioCopyIoctl     = 0xc028aa03
	uffdioZeropageIoctl = 0xc020aa04
)

// fill answers one fault by copying the page out of the memory image.
func (h *uffdHandler) fill(addr uint64) error {
	h.mu.Lock()
	fd, mappings := h.uffd, h.mappings
	h.mu.Unlock()

	m, ok := regionFor(mappings, addr)
	if !ok {
		// A fault outside every region means the layout and the guest disagree,
		// which is not something this process can paper over.
		return fmt.Errorf("uffd: fault at %#x is outside every known region", addr)
	}
	page := m.PageSize
	if page == 0 {
		page = uint64(os.Getpagesize())
	}
	base := addr & ^(page - 1)

	if h.isRemoved(base) {
		return h.zero(fd, base, page)
	}

	off := m.Offset + (base - m.BaseHostVirtAddr)
	if off+page > uint64(len(h.image)) {
		// Past the end of the image the guest is reading memory the snapshot
		// never captured; zeroes are what a fresh page would hold.
		return h.zero(fd, base, page)
	}

	req := uffdioCopy{
		Dst: base,
		Src: uint64(uintptr(unsafe.Pointer(&h.image[off]))),
		Len: page,
	}
	if err := ioctlPtr(fd, uffdioCopyIoctl, unsafe.Pointer(&req)); err != nil {
		// The guest can touch a page twice before the first answer lands; the
		// second copy is redundant rather than a failure.
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("uffd: copy page at %#x: %w", base, err)
	}
	h.faults.Add(1)
	return nil
}

func (h *uffdHandler) zero(fd int, base, page uint64) error {
	req := uffdioZeropage{RangeStart: base, RangeLen: page}
	if err := ioctlPtr(fd, uffdioZeropageIoctl, unsafe.Pointer(&req)); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("uffd: zero page at %#x: %w", base, err)
	}
	return nil
}

func ioctlPtr(fd int, req uint, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), uintptr(req), uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// regionFor finds the region containing a host virtual address.
func regionFor(mappings []uffdMapping, addr uint64) (uffdMapping, bool) {
	for _, m := range mappings {
		if addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size {
			return m, true
		}
	}
	return uffdMapping{}, false
}

func (h *uffdHandler) markRemoved(start, end uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.removed == nil {
		h.removed = map[uint64]struct{}{}
	}
	page := uint64(os.Getpagesize())
	for a := start & ^(page - 1); a < end; a += page {
		h.removed[a] = struct{}{}
	}
}

func (h *uffdHandler) isRemoved(base uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.removed[base]
	return ok
}

// Err reports a handler failure without blocking. Firecracker waits forever for
// a page nobody sends, so a caller that sees a hang needs a way to find out the
// handler is the reason.
func (h *uffdHandler) Err() error {
	select {
	case err := <-h.failed:
		return err
	default:
		return nil
	}
}

func (h *uffdHandler) fail(err error) {
	select {
	case h.failed <- err:
	default:
	}
}

// Close releases the handler. It is safe to call more than once, which matters
// because a VM can be torn down either by its own failure or by the caller.
func (h *uffdHandler) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	fd := h.uffd
	h.uffd = -1
	h.mu.Unlock()

	h.listener.Close()
	if fd >= 0 {
		// Closing the fd is what unblocks the serve loop: it has no other way
		// to notice a shutdown while parked in read.
		unix.Close(fd)
	}
	if h.image != nil {
		unix.Munmap(h.image)
		h.image = nil
	}
	return h.imageF.Close()
}

// readMsg waits for the next fault event.
//
// The descriptor arrives from Firecracker in non-blocking mode, so a plain read
// returns EAGAIN rather than waiting: poll supplies the waiting. The timeout also
// gives the loop a chance to notice Close, which it would otherwise sleep
// through.
func (h *uffdHandler) readMsg() (uffdMsg, error) {
	var msg uffdMsg
	b := (*[unsafe.Sizeof(uffdMsg{})]byte)(unsafe.Pointer(&msg))[:]
	for {
		h.mu.Lock()
		fd, closed := h.uffd, h.closed
		h.mu.Unlock()
		if closed || fd < 0 {
			return msg, os.ErrClosed
		}

		n, err := unix.Read(fd, b)
		switch {
		case err == nil:
			if n == 0 {
				return msg, os.ErrClosed
			}
			return msg, nil
		case err == unix.EINTR:
			continue
		case err == unix.EAGAIN:
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, perr := unix.Poll(fds, 200); perr != nil && perr != unix.EINTR {
				return msg, fmt.Errorf("uffd: poll: %w", perr)
			}
			continue
		default:
			return msg, fmt.Errorf("uffd: read event: %w", err)
		}
	}
}
