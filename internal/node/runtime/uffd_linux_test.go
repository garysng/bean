//go:build linux

package runtime

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The handler reads a kernel struct by offset and answers faults with raw
// ioctls, so a wrong constant would not fail loudly — it would hand the guest
// the wrong page. These values come from linux/userfaultfd.h on the host the fc
// tier runs on; the test states them independently of the code so a change to
// either side has to be deliberate.
func TestUffdMsgMatchesKernelLayout(t *testing.T) {
	if got := unsafe.Sizeof(uffdMsg{}); got != 32 {
		t.Errorf("sizeof(uffdMsg) = %d, kernel struct uffd_msg is 32", got)
	}
	// arg starts at offset 8, so a pagefault address at arg+8 is offset 16 in
	// the struct and a remove start at arg+0 is offset 8.
	if got := unsafe.Offsetof(uffdMsg{}.Arg); got != 8 {
		t.Errorf("offsetof(uffdMsg.Arg) = %d, want 8", got)
	}
	if uffdPagefaultAddrOff != 8 {
		t.Errorf("pagefault address offset within arg = %d, want 8",
			uffdPagefaultAddrOff)
	}
	if uffdEventPagefault != 0x12 {
		t.Errorf("UFFD_EVENT_PAGEFAULT = %#x, want 0x12", uffdEventPagefault)
	}
	if uffdEventRemove != 0x15 {
		t.Errorf("UFFD_EVENT_REMOVE = %#x, want 0x15", uffdEventRemove)
	}
	if uffdioCopyIoctl != 0xc028aa03 {
		t.Errorf("UFFDIO_COPY = %#x, want 0xc028aa03", uffdioCopyIoctl)
	}
	if uffdioZeropageIoctl != 0xc020aa04 {
		t.Errorf("UFFDIO_ZEROPAGE = %#x, want 0xc020aa04", uffdioZeropageIoctl)
	}
}

func TestRegionForFindsTheContainingRegion(t *testing.T) {
	mappings := []uffdMapping{
		{BaseHostVirtAddr: 0x1000, Size: 0x1000, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x5000, Size: 0x2000, Offset: 0x1000, PageSize: 4096},
	}
	for _, tc := range []struct {
		addr   uint64
		want   uint64 // expected region base, 0 = expect no match
		inside bool
	}{
		{0x1000, 0x1000, true}, // first byte of a region
		{0x1fff, 0x1000, true}, // last byte
		{0x2000, 0, false},     // one past the end: the gap is not mapped
		{0x6800, 0x5000, true}, // inside the second region
		{0x7000, 0, false},     // past the last region
		{0x0fff, 0, false},     // before the first region
	} {
		got, ok := regionFor(mappings, tc.addr)
		if ok != tc.inside {
			t.Errorf("regionFor(%#x) matched = %v, want %v", tc.addr, ok, tc.inside)
			continue
		}
		if ok && got.BaseHostVirtAddr != tc.want {
			t.Errorf("regionFor(%#x) = region at %#x, want %#x",
				tc.addr, got.BaseHostVirtAddr, tc.want)
		}
	}
}

// TestUffdHandlerRejectsEmptyImage covers a truncated bundle. Mapping a
// zero-length file would succeed and then answer every fault with garbage, so
// this has to fail at setup.
func TestUffdHandlerRejectsEmptyImage(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "memory")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newUffdHandler(filepath.Join(dir, "u.sock"), empty); err == nil {
		t.Error("accepted an empty memory image")
	}
}

func TestUffdHandlerRejectsMissingImage(t *testing.T) {
	dir := t.TempDir()
	if _, err := newUffdHandler(filepath.Join(dir, "u.sock"),
		filepath.Join(dir, "absent")); err == nil {
		t.Error("accepted a missing memory image")
	}
}

// TestUffdHandlerServesFaultsFromTheImage exercises the real path: register a
// region with the kernel, hand the fd to the handler the way Firecracker does,
// then touch the memory and check the bytes come from the image.
func TestUffdHandlerServesFaultsFromTheImage(t *testing.T) {
	// Blocking rather than non-blocking: the handler's serve loop reads the fd
	// directly and relies on read parking until an event arrives.
	r, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC, 0, 0)
	if errno != 0 {
		t.Skipf("userfaultfd unavailable: %v", errno)
	}
	fd := int(r)
	defer unix.Close(fd)

	page := os.Getpagesize()
	dir := t.TempDir()

	// The image holds a recognisable pattern, so a served page proves the data
	// came from the file rather than from zero-filled anonymous memory.
	want := make([]byte, page)
	for i := range want {
		want[i] = byte('a' + i%26)
	}
	imgPath := filepath.Join(dir, "memory")
	if err := os.WriteFile(imgPath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := uffdAPI(fd); err != nil {
		t.Skipf("UFFDIO_API: %v", err)
	}

	// Anonymous memory standing in for the guest's, registered for faults just
	// as Firecracker registers the regions it maps.
	guest, err := unix.Mmap(-1, 0, page, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(guest)
	base := uint64(uintptr(unsafe.Pointer(&guest[0])))
	if err := uffdRegister(fd, base, uint64(page)); err != nil {
		t.Skipf("UFFDIO_REGISTER: %v", err)
	}

	sock := filepath.Join(dir, "u.sock")
	h, err := newUffdHandler(sock, imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Firecracker's side of the handshake: the region layout as the body, the
	// userfault fd as control data.
	layout, err := json.Marshal([]uffdMapping{{
		BaseHostVirtAddr: base, Size: uint64(page), Offset: 0,
		PageSize: uint64(page),
	}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	uc := conn.(*net.UnixConn)
	rights := unix.UnixRights(fd)
	if _, _, err := uc.WriteMsgUnix(layout, rights, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// Touching the page faults, which the handler must answer.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if guest[0] != 0 {
			break
		}
		if err := h.Err(); err != nil {
			t.Fatalf("handler failed: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("page was never filled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if string(guest) != string(want) {
		t.Errorf("served page does not match the image (first bytes %q)", guest[:16])
	}
}

// uffdAPI performs the UFFDIO_API handshake with the kernel.
func uffdAPI(fd int) error {
	var api struct {
		API      uint64
		Features uint64
		Ioctls   uint64
	}
	api.API = 0xAA
	return ioctlPtr(fd, 0xc018aa3f, unsafe.Pointer(&api))
}

func uffdRegister(fd int, start, length uint64) error {
	var reg struct {
		RangeStart uint64
		RangeLen   uint64
		Mode       uint64
		Ioctls     uint64
	}
	reg.RangeStart = start
	reg.RangeLen = length
	reg.Mode = 1 // UFFDIO_REGISTER_MODE_MISSING
	return ioctlPtr(fd, 0xc020aa00, unsafe.Pointer(&reg))
}
