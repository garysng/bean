//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/vsock"
)

// agentVsockPort is the port beand listens on inside the guest. It is fixed
// rather than allocated: each VM has its own vsock namespace, so there is
// nothing to collide with, and a constant keeps the guest's command line
// independent of host state.
const agentVsockPort = 1024

// guestCID is the context id assigned to every guest. Like the port, it is
// per-VM and so needs no allocation. 3 is the lowest id available to guests —
// 0 through 2 are reserved by the vsock protocol.
const guestCID = 3

// guestRootfsDevice is where the user image appears inside the guest. Firecracker
// names drives in attachment order, so this holds as long as the agent disk is
// registered first.
const guestRootfsDevice = "/dev/vdb"

// FCRuntime runs each sandbox as a Firecracker microVM.
//
// The isolation boundary is a virtual machine rather than a namespace, which is
// what makes it safe to run untrusted code: an escape needs a KVM or device
// model bug, not a container misconfiguration. The cost is that a rootfs must
// be a block device and the agent must be reachable without host networking,
// which is why this runtime depends on an image.Provider and vsock.
type FCRuntime struct {
	// FirecrackerBin is the VMM binary.
	FirecrackerBin string
	// KernelPath is the guest kernel image, shared by every sandbox.
	KernelPath string
	// AgentDiskPath is a read-only image holding the beand binary, attached as
	// a second drive. Shipping the agent this way means it upgrades with the
	// node rather than requiring every user image to embed it.
	AgentDiskPath string
	// BaseDir holds per-sandbox runtime state: API socket, vsock socket, logs.
	BaseDir string
	// Images supplies the rootfs block device.
	Images image.Provider
	// Committer seals a sandbox's filesystem into a new base image. Nil
	// disables commit, which is what a node that only runs sandboxes wants.
	Committer *image.Committer

	mu   sync.Mutex
	vms  map[string]*fcVM
	once sync.Once
}

// fcVM is one running microVM.
type fcVM struct {
	id     string
	dir    string
	cmd    *exec.Cmd
	client *fcClient
	rootfs *image.Rootfs
	paused bool
	// done closes when the VMM process exits, so waiters do not poll.
	done chan struct{}
}

// Names inside a sandbox directory. Every path Firecracker records — the vsock
// UDS and both drives — is relative to that directory, so a snapshot taken by
// one sandbox restores into another. See startVMM.
const (
	vsockName     = "vsock.sock"
	agentDiskName = "agent.ext4"
)

// vsockHostPath is where callers on the host find the socket.
func (v *fcVM) vsockHostPath() string { return filepath.Join(v.dir, vsockName) }

func NewFCRuntime(fcBin, kernel, agentDisk, baseDir string, images image.Provider) *FCRuntime {
	return &FCRuntime{
		FirecrackerBin: fcBin,
		KernelPath:     kernel,
		AgentDiskPath:  agentDisk,
		BaseDir:        baseDir,
		Images:         images,
		vms:            map[string]*fcVM{},
	}
}

func (r *FCRuntime) Name() string { return "fc" }

// PrewarmImage makes an image ready ahead of a sandbox, so a create does not
// pay for a first pull.
func (r *FCRuntime) PrewarmImage(ctx context.Context, imageRef string) error {
	if r.Images == nil {
		return errors.New("fc: no image provider")
	}
	return r.Images.Prewarm(ctx, imageRef)
}

// CachedImages reports the images available on this node.
func (r *FCRuntime) CachedImages() (map[string]int64, error) {
	if r.Images == nil {
		return nil, errors.New("fc: no image provider")
	}
	return r.Images.Cached()
}

// CommitSandbox seals a sandbox's filesystem into a base image under tag.
//
// The sandbox must be paused so the filesystem is not moving underneath the
// read; the caller owns that, since only it knows whether the sandbox should
// keep running afterwards.
func (r *FCRuntime) CommitSandbox(ctx context.Context, id, tag string) error {
	if r.Committer == nil {
		return errors.New("fc: commit not configured")
	}
	vm, err := r.get(id)
	if err != nil {
		return err
	}
	_, err = r.Committer.Commit(ctx, vm.rootfs.Device, tag)
	return err
}

func (r *FCRuntime) Create(ctx context.Context, spec *Spec) (*Handle, error) {
	return r.create(ctx, spec, nil)
}

// Restore boots a VM from a Firecracker snapshot. The guest resumes with its
// memory intact, so a restored sandbox keeps running processes and open files —
// the property that makes resume cheap compared to a cold start.
func (r *FCRuntime) Restore(ctx context.Context, spec *Spec, src io.Reader) (*Handle, error) {
	return r.create(ctx, spec, src)
}

func (r *FCRuntime) create(ctx context.Context, spec *Spec, restoreFrom io.Reader) (handle *Handle, err error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if spec == nil || spec.SandboxID == "" {
		return nil, errors.New("fc: sandbox id required")
	}

	r.mu.Lock()
	if _, exists := r.vms[spec.SandboxID]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("fc: sandbox %s already exists", spec.SandboxID)
	}
	r.mu.Unlock()

	dir := filepath.Join(r.BaseDir, spec.SandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fc: create sandbox dir: %w", err)
	}

	// Everything below can fail partway through. Cleanup is registered as it
	// goes so a failed create leaves no VMM process, no device and no files —
	// an orphaned microVM holds memory the scheduler thinks is free.
	var cleanup []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	cleanup = append(cleanup, func() { os.RemoveAll(dir) })

	rootfs, err := r.Images.Prepare(ctx, spec.SandboxID, spec.Image, spec.DiskMiB)
	if err != nil {
		return nil, fmt.Errorf("fc: prepare rootfs: %w", err)
	}
	cleanup = append(cleanup, func() { _ = rootfs.Release() })

	// The rootfs must sit in the sandbox directory for the relative drive path
	// to resolve. The provider is free to put it elsewhere, so this is checked
	// rather than assumed: a mismatch would only surface as a failed restore.
	if filepath.Dir(rootfs.Device) != dir {
		return nil, fmt.Errorf("fc: rootfs %s is not in the sandbox directory %s",
			rootfs.Device, dir)
	}

	// A symlink gives the shared agent disk a name inside this sandbox, so its
	// drive path can be relative like the rootfs. One inode, no copy.
	if err := os.Symlink(r.AgentDiskPath, filepath.Join(dir, agentDiskName)); err != nil {
		return nil, fmt.Errorf("fc: link agent disk: %w", err)
	}

	vm := &fcVM{
		id:     spec.SandboxID,
		dir:    dir,
		rootfs: rootfs,
		done:   make(chan struct{}),
	}

	apiSocket := filepath.Join(dir, "api.sock")
	if err := r.startVMM(ctx, vm, apiSocket); err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { r.killVMM(vm) })

	vm.client = newFCClient(apiSocket)
	if err := waitAPIReady(ctx, apiSocket); err != nil {
		return nil, fmt.Errorf("fc: api socket: %w", err)
	}

	if restoreFrom != nil {
		if err = r.loadSnapshot(ctx, vm, restoreFrom); err != nil {
			return nil, err
		}
	} else {
		if err = r.configureAndBoot(ctx, vm, spec); err != nil {
			return nil, err
		}
	}

	r.mu.Lock()
	r.vms[spec.SandboxID] = vm
	r.mu.Unlock()

	return &Handle{
		SandboxID:  spec.SandboxID,
		AgentAddr:  vsock.Addr{SocketPath: vm.vsockHostPath(), Port: agentVsockPort}.Target(),
		StartedAt:  time.Now(),
		PID:        vm.cmd.Process.Pid,
		RuntimeTag: r.Name(),
	}, nil
}

func (r *FCRuntime) validate() error {
	if r.FirecrackerBin == "" {
		return errors.New("fc: firecracker binary path required")
	}
	if r.KernelPath == "" {
		return errors.New("fc: kernel path required")
	}
	// The agent disk is the guest's root device, so it is not optional: without
	// it the kernel has no init to exec.
	if r.AgentDiskPath == "" {
		return errors.New("fc: agent disk required (it is the guest root device)")
	}
	if r.Images == nil {
		return errors.New("fc: image provider required")
	}
	return nil
}

// startVMM launches Firecracker with its API socket. The process is its own
// group leader so a kill reaches everything it spawned, and its console goes
// to a file: a guest that fails to boot leaves no other evidence.
func (r *FCRuntime) startVMM(ctx context.Context, vm *fcVM, apiSocket string) error {
	logFile, err := os.OpenFile(filepath.Join(vm.dir, "console.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("fc: open console log: %w", err)
	}
	defer logFile.Close()

	// The context is deliberately not passed to CommandContext: the VM must
	// outlive the request that created it.
	cmd := exec.Command(r.FirecrackerBin, "--api-sock", apiSocket)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The working directory is the sandbox's own, which is what makes the vsock
	// UDS path relative and therefore portable across a restore: Firecracker
	// saves that path into the machine state and refuses to override it on load,
	// so a relative path is the only way a snapshot taken by one sandbox can be
	// restored into another.
	cmd.Dir = vm.dir

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fc: start firecracker: %w", err)
	}
	vm.cmd = cmd

	go func() {
		_ = cmd.Wait()
		close(vm.done)
	}()
	return nil
}

// configureAndBoot sets up a fresh machine and starts it.
func (r *FCRuntime) configureAndBoot(ctx context.Context, vm *fcVM, spec *Spec) error {
	vcpus := int64(spec.CPU)
	if vcpus < 1 {
		vcpus = 1
	}
	mem := spec.MemoryMiB
	if mem <= 0 {
		mem = 512
	}

	if err := vm.client.put(ctx, "/machine-config", fcMachineConfig{
		VCPUCount: vcpus, MemSizeMiB: mem,
	}); err != nil {
		return err
	}

	// The agent disk boots as the root device and the user image is attached
	// beside it. The kernel execs init from whatever it mounted as root, so
	// putting the agent there is what keeps user images free of any obligation
	// to embed beand or an init system: the agent pivots to the user rootfs
	// once it is running.
	//
	// Serial console output is kept, since a boot failure leaves no other
	// evidence. Panic reboots are disabled so a crashed guest stays
	// inspectable rather than looping.
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=-1 pci=off init=/bean/beand -- --listen vsock:%d --pivot %s",
		agentVsockPort, guestRootfsDevice)
	if err := vm.client.put(ctx, "/boot-source", fcBootSource{
		KernelImagePath: r.KernelPath, BootArgs: bootArgs,
	}); err != nil {
		return err
	}

	// Drive order determines device naming in the guest: the first attached
	// drive is /dev/vda, the second /dev/vdb. guestRootfsDevice depends on
	// that, so the agent disk must be registered first.
	//
	// Both paths are relative to the VMM's working directory. Firecracker saves
	// device paths into the machine state and resolves them again on restore,
	// so an absolute path would send a restored VM looking for the source
	// sandbox's files. Relative paths resolve inside whichever sandbox
	// directory the VMM was started in. The agent disk is symlinked in for the
	// same reason.
	if err := vm.client.put(ctx, "/drives/agent", fcDrive{
		DriveID: "agent", PathOnHost: agentDiskName,
		IsRootDevice: true, IsReadOnly: true,
	}); err != nil {
		return err
	}

	if err := vm.client.put(ctx, "/drives/rootfs", fcDrive{
		DriveID: "rootfs", PathOnHost: filepath.Base(vm.rootfs.Device),
		IsRootDevice: false, IsReadOnly: vm.rootfs.ReadOnly,
	}); err != nil {
		return err
	}

	if err := vm.client.put(ctx, "/vsock", fcVsock{
		// Relative to the VMM's working directory, which is this sandbox's
		// state directory: that is what survives a snapshot/restore.
		GuestCID: guestCID, UDSPath: vsockName,
	}); err != nil {
		return err
	}

	return vm.client.put(ctx, "/actions", fcAction{ActionType: "InstanceStart"})
}

// waitAPIReady blocks until Firecracker has created its API socket. Sending a
// request before then fails with a confusing connection error.
func waitAPIReady(ctx context.Context, apiSocket string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(apiSocket); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("firecracker did not create its api socket")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
