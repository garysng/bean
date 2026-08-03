//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"
)

// The tests below drive configureAndBoot against a stand-in for Firecracker's API
// socket. What they are guarding is ordering and absence, neither of which the
// real VMM reports usefully: registering a NIC too late is refused with a message
// no caller reads, and omitting one entirely is not an error at all -- it surfaces
// as pip and git failing inside a guest that came up looking healthy.

// fcRecorder answers like Firecracker and records the sequence it was asked in.
type fcRecorder struct {
	socket string
	srv    *http.Server

	mu   sync.Mutex
	reqs []recordedReq
}

type recordedReq struct {
	method string
	path   string
	body   []byte
}

func startFCRecorder(t *testing.T) *fcRecorder {
	t.Helper()
	// Under the socket-length limit the kernel imposes on AF_UNIX paths, which a
	// long t.TempDir() plus a nested name can otherwise exceed.
	dir, err := os.MkdirTemp("", "fcrec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	rec := &fcRecorder{socket: filepath.Join(dir, "api.sock")}
	ln, err := net.Listen("unix", rec.socket)
	if err != nil {
		t.Fatal(err)
	}
	rec.srv = &http.Server{Handler: rec}
	go func() { _ = rec.srv.Serve(ln) }()
	t.Cleanup(func() { _ = rec.srv.Close() })
	return rec
}

func (rec *fcRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, recordedReq{method: r.Method, path: r.URL.Path, body: body})
	rec.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// paths returns the request paths in the order they arrived.
func (rec *fcRecorder) paths() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, 0, len(rec.reqs))
	for _, r := range rec.reqs {
		out = append(out, r.path)
	}
	return out
}

// indexOf reports where a path appeared, or -1. Order is the point of these
// tests, so it is compared as position rather than membership.
func (rec *fcRecorder) indexOf(path string) int {
	for i, p := range rec.paths() {
		if p == path {
			return i
		}
	}
	return -1
}

// bodyOf returns the body sent to a path, or false if it was never requested.
//
// The lock is released before reporting, because the report wants the request
// list and paths() takes the same lock. Written as a lookup returning false
// rather than one that fails in place: the first version called t.Fatalf while
// holding the mutex and self-deadlocked, so the suite hung for the full test
// timeout instead of naming the missing NIC.
func (rec *fcRecorder) bodyOf(path string) ([]byte, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.reqs {
		if r.path == path {
			return r.body, true
		}
	}
	return nil, false
}

// requireBody fails with the recorded sequence when a path was never requested.
func (rec *fcRecorder) requireBody(t *testing.T, path string) []byte {
	t.Helper()
	body, ok := rec.bodyOf(path)
	if !ok {
		t.Fatalf("no request to %s; got %v", path, rec.paths())
	}
	return body
}

// bootVMAgainst runs configureAndBoot with layout, returning the recorder.
func bootVMAgainst(t *testing.T, layout *network.Layout) *fcRecorder {
	t.Helper()
	rec := startFCRecorder(t)
	dir := t.TempDir()

	rt := &FCRuntime{KernelPath: "/does/not/need/to/exist/vmlinux"}
	vm := &fcVM{
		id:     "sb-net",
		dir:    dir,
		client: newFCClient(rec.socket),
		rootfs: &image.Rootfs{Device: filepath.Join(dir, "rootfs.img")},
	}
	spec := &Spec{SandboxID: "sb-net", CPU: 1, MemoryMiB: 512, Network: layout}

	if err := rt.configureAndBoot(context.Background(), vm, spec); err != nil {
		t.Fatalf("configureAndBoot: %v", err)
	}
	return rec
}

func testLayout(t *testing.T) *network.Layout {
	t.Helper()
	layout, err := network.LayoutFor(7, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

// TestConfigureAndBootRegistersNICBeforeStart pins the ordering constraint. The
// endpoint is pre-boot only, so a NIC registered after InstanceStart is refused
// and the guest runs its whole life without one.
func TestConfigureAndBootRegistersNICBeforeStart(t *testing.T) {
	layout := testLayout(t)
	rec := bootVMAgainst(t, layout)

	const nicPath = "/network-interfaces/" + guestIfaceID
	nic := rec.indexOf(nicPath)
	if nic < 0 {
		t.Fatalf("no NIC registered with a layout present; requests: %v", rec.paths())
	}
	start := rec.indexOf("/actions")
	if start < 0 {
		t.Fatalf("machine never started; requests: %v", rec.paths())
	}
	if nic > start {
		t.Errorf("NIC registered at position %d, after InstanceStart at %d: "+
			"Firecracker refuses a network device on a running machine, so the "+
			"guest would boot with no interface; requests: %v", nic, start, rec.paths())
	}

	var got fcNetworkInterface
	if err := json.Unmarshal(rec.requireBody(t, nicPath), &got); err != nil {
		t.Fatalf("decode NIC request: %v", err)
	}
	if got.HostDevName != layout.TapName {
		t.Errorf("host_dev_name = %q, want the tap from the layout %q",
			got.HostDevName, layout.TapName)
	}
	if got.IfaceID != guestIfaceID {
		t.Errorf("iface_id = %q, want %q", got.IfaceID, guestIfaceID)
	}
}

// TestConfigureAndBootOmitsMACAndMTU guards the wire shape rather than the Go
// zero values. Firecracker only advertises VIRTIO_NET_F_MAC when guest_mac is
// present, so sending an empty string is not the same as sending nothing: it
// changes what the guest driver does, or is rejected as an unparseable address.
func TestConfigureAndBootOmitsMACAndMTU(t *testing.T) {
	rec := bootVMAgainst(t, testLayout(t))

	var raw map[string]any
	body := rec.requireBody(t, "/network-interfaces/"+guestIfaceID)
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode NIC request: %v", err)
	}
	for _, key := range []string{"guest_mac", "mtu"} {
		if _, present := raw[key]; present {
			t.Errorf("NIC request carries %q (%s); it must be absent so the guest "+
				"driver picks its own address and MTU", key, body)
		}
	}
}

// TestConfigureAndBootWithoutNetworkRegistersNoNIC is the backwards-compatible
// path: a node with no networking configured has to boot sandboxes exactly as it
// did before this existed, with no interface and no failure.
func TestConfigureAndBootWithoutNetworkRegistersNoNIC(t *testing.T) {
	rec := bootVMAgainst(t, nil)

	for _, p := range rec.paths() {
		if strings.HasPrefix(p, "/network-interfaces") {
			t.Errorf("registered %s with no layout; a node without networking must "+
				"behave as it did before", p)
		}
	}
	// The absence has to be the only difference. A test that only checked for no
	// NIC would pass just as well if the whole boot had stopped early.
	for _, want := range []string{"/machine-config", "/boot-source", "/drives/agent",
		"/drives/rootfs", "/vsock", "/actions"} {
		if rec.indexOf(want) < 0 {
			t.Errorf("missing %s; the boot sequence changed rather than only "+
				"dropping the NIC. requests: %v", want, rec.paths())
		}
	}
}

// TestLoadSnapshotWithoutMemoryRegistersNIC covers the branch that looks like a
// restore but is a boot. A filesystem-only checkpoint has no machine state to
// carry an interface, so its guest needs one registered like any cold start --
// and this is the case where "restore does not need network_overrides" would be
// the wrong conclusion to apply.
func TestLoadSnapshotWithoutMemoryRegistersNIC(t *testing.T) {
	rec := startFCRecorder(t)
	dir := t.TempDir()

	rt := &FCRuntime{KernelPath: "/does/not/need/to/exist/vmlinux"}
	vm := &fcVM{
		id:     "sb-fsonly",
		dir:    dir,
		client: newFCClient(rec.socket),
		rootfs: &image.Rootfs{Device: filepath.Join(dir, "rootfs.img")},
	}
	layout := testLayout(t)
	spec := &Spec{SandboxID: "sb-fsonly", CPU: 1, MemoryMiB: 512, Network: layout}

	// An entry with no memory image is what a checkpoint taken without memory
	// unpacks to, and loadSnapshot dispatches on exactly that.
	stage := &snapshotStage{entry: snapEntry{StatePath: "", MemPath: ""}}
	if err := rt.loadSnapshot(context.Background(), vm, spec, stage); err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}

	if rec.indexOf("/network-interfaces/"+guestIfaceID) < 0 {
		t.Errorf("filesystem-only restore booted without a NIC; requests: %v", rec.paths())
	}
	if rec.indexOf("/snapshot/load") >= 0 {
		t.Errorf("a checkpoint with no memory must boot rather than load; requests: %v",
			rec.paths())
	}
}
