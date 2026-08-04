//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"
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

// guestIfaceID names the sandbox's only network interface. Like the vsock CID it
// is a constant because there is nothing to collide with: the id is internal to
// Firecracker, one per machine, and it is the key a network_overrides entry would
// have to match on restore. Deriving it from the sandbox would make that key
// depend on which sandbox took the snapshot.
const guestIfaceID = "eth0"

// guestIPBootArg renders the kernel's ip= parameter for a sandbox's layout, or ""
// when the sandbox has no network.
//
// Registering the NIC gives the guest a device; it does not give it an address.
// Without this the guest boots with eth0 present, down, and unaddressed, which is
// the state every assertion in this package tolerates: the NIC is registered before
// InstanceStart, the VMM is in the right namespace, the tap exists, the host rules
// are installed, and nothing can reach anything.
//
// The kernel does the configuring, from the command line, before init runs. That is
// the reason for choosing it over having the agent do the work: the agent is PID 1
// and does several things before it is reachable -- it pivots to the user rootfs and
// binds a vsock listener -- so an interface configured there would be absent during
// the guest's own early startup. It also means a guest whose agent fails still has a
// network to debug over.
//
// The form is the full colon-separated one rather than ip=dhcp, because there is no
// DHCP server in the namespace. Fields are
// client:server:gateway:netmask:hostname:device:autoconf. The empty server and
// hostname are deliberate: there is no NFS root, and the guest's name is not the
// network's business. The trailing "off" stops the kernel probing for DHCP or BOOTP,
// which on a link with nothing listening costs seconds of boot before it gives up.
//
// Every value is rendered from a net.IP or a constant, so none can contain the
// whitespace the kernel command line splits on and does not let anything quote.
func guestIPBootArg(l *network.Layout) string {
	if l == nil {
		return ""
	}
	mask := net.IP(l.GuestSubnet.Mask).String()
	return fmt.Sprintf(" ip=%s::%s:%s::%s:off",
		l.GuestIP, l.GuestGateway, mask, guestIfaceID)
}

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
	// Builder builds images from Dockerfiles. Nil disables builds on this node,
	// which is the right default: building needs BuildKit, and a cluster may
	// prefer dedicated builder nodes over the dependency everywhere.
	Builder *image.Builder
	// DebugConsole attaches the guest kernel to the serial port. It is off by
	// default because the 8250 UART is synchronous: the guest blocks on every
	// log line it emits. Measured on a 6.1 guest, dropping the console took the
	// time from VMM start to a reachable agent from 1193ms to 700ms.
	//
	// The kernel still has the driver, so turning this on needs no new kernel —
	// which is the point: a boot that fails leaves no other evidence, and this
	// is how that evidence is recovered.
	DebugConsole bool
	// GuestDNS is the resolver the in-guest agent writes into the guest's
	// /etc/resolv.conf. Empty leaves the user image's own file alone.
	//
	// Carried here because the boot arguments are the only channel to the agent:
	// it is PID 1, started by the kernel, with no environment and no config file.
	// FCConfig validated and logged this value before this field existed and then
	// dropped it, so a node configured with a resolver produced guests that could
	// not resolve and said nothing about why.
	GuestDNS string

	// CPUTemplate masks CPU features from guests so a memory snapshot is not
	// bound to the host that produced it. See cpu_template.go — it must be set
	// before a guest boots, so it is node configuration and not a per-snapshot
	// choice.
	CPUTemplate CPUTemplate

	// SnapshotCache bounds the unpacked snapshot cache. The zero value leaves it
	// unbounded, which is the historical behaviour and measured 4.6 GB across nine
	// entries on a development node — invisible to the scheduler, since the cache
	// consumes no commitment.
	SnapshotCache EvictionPolicy

	// TrackDirtyPages lets guests on this node produce diff checkpoints, which
	// capture only the memory written since their base.
	//
	// Like CPUTemplate this is node configuration rather than a per-checkpoint
	// choice, for a harder reason: KVM has to be logging writes from the moment
	// the guest starts, and a snapshot does not carry the setting. A guest booted
	// without it can never produce a diff, however the checkpoint is requested.
	//
	// Off by default until the cost of that logging is measured on real
	// workloads. Every guest pays it, while only some sandboxes are ever
	// checkpointed more than once.
	TrackDirtyPages bool

	// Cgroups holds each sandbox's VMM in a cgroup with a memory ceiling, a CPU
	// quota and a pid cap derived from that sandbox's own spec. Nil applies no
	// limits, which is what a node that has not configured it gets and is the
	// behaviour every existing deployment is running.
	//
	// This is what overcommit.go and cmd/noded/main.go name as the prerequisite for
	// raising memory overcommit above 1.0: without it the committed quantity is the
	// scheduler's ledger and nothing in the kernel enforces it.
	Cgroups *cgroupHost

	// VMMCreds drops the VMM to an unprivileged uid. Nil runs it with noded's own
	// credentials, which is root -- see vmmcreds.go for what the drop does and does
	// not buy without a mount namespace.
	VMMCreds *vmmCreds

	// snapshots holds unpacked snapshot state, so restoring the same checkpoint
	// twice does not unpack it twice.
	snapshots *snapCache
	// warm holds one checkpoint per (image digest, CPU) so a create can restore
	// instead of booting. See warmstore_linux.go.
	warm *warmStore
	// WarmSnapshots enables producing and consulting them.
	WarmSnapshots bool
	// WarmEviction bounds the warm store. The zero value leaves it unbounded, which
	// grows by roughly one guest's memory per image per CPU generation and is
	// invisible to the scheduler -- a node can fill its disk while placement still
	// believes it has room.
	//
	// Separate from SnapshotCache rather than sharing its budget, because evicting
	// the wrong one costs differently: a snapshot-cache entry can be re-unpacked from
	// the control plane's blob, while a warm bundle can only be rebuilt by booting a
	// guest again. A burst of restores must not evict the bundles that make creates
	// cheap.
	WarmEviction EvictionPolicy

	mu   sync.Mutex
	vms  map[string]*fcVM
	once sync.Once
}

// fcVM is one running microVM.
type fcVM struct {
	id string
	// imageRef is what this sandbox was started from. Kept because a commit has to
	// carry the source image's configuration onto its output, and by then the spec
	// that named the image is gone.
	imageRef string
	dir      string
	cmd      *exec.Cmd
	client   *fcClient
	rootfs   *image.Rootfs
	paused   bool
	// uffd serves guest page faults for a VM restored from a snapshot. Nil for a
	// cold boot, which has no memory image to fault against.
	uffd *uffdHandler
	// dirtyPages reports whether KVM is logging this guest's writes, which is
	// what a diff checkpoint needs. It is set when the VM starts and cannot
	// change afterwards, so it is the authority on whether a diff is possible.
	dirtyPages bool
	// cgroup holds this VMM's resource limits. Nil on a node with no cgroup
	// support or none configured.
	//
	// Carried on the VM rather than looked up again at teardown, because the
	// directories it names are the only record that they exist: a Destroy that
	// recomputed the path from the id would work, but a create that failed halfway
	// through building the group would not have a path to recompute. That is the
	// leak in GitHub #16 -- a resource nothing afterwards knows about.
	cgroup *sandboxCgroup
	// netnsPath is the handle of the network namespace the VMM runs in, or "" on
	// a node with no networking. It is carried on the VM rather than read from
	// the Spec inside startVMM because startVMM is given the VM, matching how
	// dir and rootfs reach it; and it is a path rather than a name so that the
	// jailer work in GitHub #20 can pass the same value to --netns.
	netnsPath string
	// done closes when the VMM process exits, so waiters do not poll.
	done chan struct{}
}

// Names inside a sandbox directory. Every path Firecracker records — the vsock
// UDS and both drives — is relative to that directory, so a snapshot taken by
// one sandbox restores into another. See startVMM.
const (
	vsockName     = "vsock.sock"
	agentDiskName = "agent.ext4"
	uffdSockName  = "uffd.sock"
)

// vsockHostPath is where callers on the host find the socket.
func (v *fcVM) vsockHostPath() string { return filepath.Join(v.dir, vsockName) }

// uffdHostPath is where the page-fault handler listens for Firecracker.
func (v *fcVM) uffdHostPath() string { return filepath.Join(v.dir, uffdSockName) }

func NewFCRuntime(fcBin, kernel, agentDisk, baseDir string, images image.Provider) *FCRuntime {
	return &FCRuntime{
		FirecrackerBin: fcBin,
		KernelPath:     kernel,
		AgentDiskPath:  agentDisk,
		BaseDir:        baseDir,
		Images:         images,
		vms:            map[string]*fcVM{},
		// Beside the sandboxes rather than inside any one of them: the entries
		// outlive the sandbox that first unpacked them.
		snapshots: newSnapCache(filepath.Join(baseDir, ".snapshots")),
		// Separate from .snapshots even though both hold bundles, because the two
		// have different lifetimes: a snapshot-cache entry is a derived copy that
		// can be re-unpacked from the control plane's blob, while a warm bundle is
		// the only copy of itself. Sweeping the first is free; sweeping the second
		// costs a boot.
		warm: newWarmStore(filepath.Join(baseDir, ".warm")),
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

// SnapshotCacheBytes reports the space held by unpacked snapshots.
func (r *FCRuntime) SnapshotCacheBytes() (int64, error) {
	return r.snapshots.Usage()
}

// CachedImages reports the images available on this node, with the size and
// digest of each, and whether a create of it here would restore rather than boot.
//
// The warm flag is added here rather than by the image package because it is the
// one place both halves are known: the provider owns the image files and their
// digests, the warm store owns the bundles, and answering "is this image warm on
// this node" needs the digest to build the key.
// ImageConfig reports what an image declared, so a create can honour its ENV,
// ENTRYPOINT, CMD and WORKDIR.
func (r *FCRuntime) ImageConfig(imageRef string) (*image.Config, error) {
	if r.Images == nil {
		return nil, errors.New("fc: no image provider")
	}
	return r.Images.Config(imageRef)
}

func (r *FCRuntime) CachedImages() (map[string]image.CachedImage, error) {
	if r.Images == nil {
		return nil, errors.New("fc: no image provider")
	}
	cached, err := r.Images.Cached()
	if err != nil {
		return nil, err
	}
	if !r.WarmSnapshots {
		return cached, nil
	}
	vendor, family, cpuErr := HostCPUIdentity()
	if cpuErr != nil {
		// Reported as not-warm rather than as an error. The images are genuinely
		// cached and that half of the answer is still useful for affinity; a node
		// that cannot identify its own CPU simply cannot claim any warm snapshot,
		// which is the safe direction -- a create placed here boots.
		slog.Warn("cannot identify host cpu; reporting no warm snapshots",
			logging.KeyError, cpuErr)
		return cached, nil
	}
	for ref, img := range cached {
		if img.Digest == "" {
			continue
		}
		key := warmKey{Digest: img.Digest, Vendor: vendor, Family: family,
			Template: r.CPUTemplate}
		if _, ok := r.warm.Lookup(key); ok {
			img.Warm = true
			cached[ref] = img
		}
	}
	return cached, nil
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
	_, err = r.Committer.Commit(ctx, vm.rootfs.Device, tag, vm.imageRef)
	return err
}

// BuildImage builds a base image from a Dockerfile on this node.
func (r *FCRuntime) BuildImage(ctx context.Context, req BuildRequest) (string, error) {
	if r.Builder == nil {
		return "", errors.New("fc: builds not configured on this node")
	}
	if _, err := r.Builder.Build(ctx, image.BuildRequest{
		Tag:        req.Tag,
		Dockerfile: req.Dockerfile,
		ContextTar: req.ContextTar,
		BuildArgs:  req.BuildArgs,
		SizeMiB:    req.SizeMiB,
		Logs:       req.Logs,
	}); err != nil {
		return "", err
	}
	return req.Tag, nil
}

func (r *FCRuntime) Create(ctx context.Context, spec *Spec) (*Handle, error) {
	return r.create(ctx, spec, nil)
}

// Fork boots a VM from a Firecracker snapshot. The guest resumes with its
// memory intact, so the new sandbox keeps running processes and open files —
// the property that makes this cheap compared to a cold start.
//
// One checkpoint can be forked repeatedly and concurrently. The unpacked memory
// image is mapped read-only and shared between them, while the writable rootfs
// layer is extracted per sandbox, so siblings diverge the moment either writes
// and neither can observe the other.
//
// Layers are the checkpoint's chain, base first. One layer is the common case;
// more than one means the leaf is incremental and its memory has to be
// reassembled from its ancestors before the guest can run.
func (r *FCRuntime) Fork(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error) {
	if len(layers) == 0 {
		return nil, errors.New("fc: fork needs at least one snapshot layer")
	}
	return r.create(ctx, spec, layers)
}

func (r *FCRuntime) create(ctx context.Context, spec *Spec, layers []SnapshotLayer) (handle *Handle, err error) {
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

	// A restore's bundle is unpacked before the rootfs is prepared, so the
	// writable layer can be seeded while the provider is still assembling it.
	// See snapstage_linux.go for why the order is load-bearing.
	var stage *snapshotStage
	if len(layers) > 0 {
		stage, err = r.stageSnapshot(filepath.Join(dir, "stage"), spec, layers)
		if err != nil {
			return nil, err
		}
		defer stage.Close()
	}

	rootfs, err := r.Images.Prepare(ctx, spec.SandboxID, spec.Image, image.PrepareOptions{
		SizeMiB:      spec.DiskMiB,
		SeedWritable: stage.SeedWritable,
	})
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

	// The cgroup is built before the VMM starts, and its limits are written before
	// anything is put in it: a process added to an unconfigured group runs
	// unbounded for as long as the writes take.
	//
	// Registered for cleanup immediately. A create that fails after this point must
	// not leave the directory behind -- an orphaned cgroup is the same class of leak
	// as GitHub #16's loop devices, invisible to everything and permanent until the
	// host reboots.
	cg, err := r.Cgroups.createCgroup(spec.SandboxID, limitsFor(spec))
	if err != nil {
		return nil, fmt.Errorf("fc: create cgroup: %w", err)
	}
	cleanup = append(cleanup, func() { _ = cg.Remove() })

	// Ownership of everything the dropped uid has to open. No-ops when no uid is
	// configured.
	//
	// The tree first, which covers the sandbox directory itself (Firecracker
	// creates its API socket and the vsock UDS in it) and the staged files. Then
	// the rootfs device separately: rootfs.img is a symlink to /dev/mapper and the
	// walk deliberately does not follow it, so the device node is chowned by name.
	// A dropped uid that cannot open its own rootfs fails at boot with the guest
	// unable to find its root device.
	if err = r.VMMCreds.chownTree(dir); err != nil {
		return nil, fmt.Errorf("fc: hand sandbox dir to the VMM uid: %w", err)
	}
	if err = r.chownRootfsDevice(rootfs); err != nil {
		return nil, err
	}

	vm := &fcVM{
		id:       spec.SandboxID,
		imageRef: spec.Image,
		dir:      dir,
		rootfs:   rootfs,
		done:     make(chan struct{}),
		// Resolved here, where the Spec is in hand. Empty on a node with no
		// network pool, which keeps that node's launch identical to before.
		netnsPath: netnsPathFor(spec),
		cgroup:    cg,
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

	if stage != nil {
		if err = r.loadSnapshot(ctx, vm, spec, stage); err != nil {
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
//
// When the sandbox has networking, the VMM is launched inside that sandbox's
// network namespace. It has to be: the tap lives in the namespace and is
// addressed by name, so a VMM in the host namespace cannot see the device the
// NIC registration names. See netns_linux.go for why the join is done with a
// pinned thread rather than by wrapping the command.
//
// The confinement, when configured, is applied in a fixed order: the credential
// drop is set on the command before the fork, the pid goes into the cgroup as soon
// as it exists, and the rlimits are set on that pid. See vmmcreds_linux.go for why
// the rlimits cannot be applied before the fork and why the window that leaves is
// harmless here.
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
	// Added to the same SysProcAttr rather than replacing it: Setpgid has to
	// survive, because killVMM signals the negative pid and depends on the VMM
	// leading its own group.
	applyCreds(cmd, r.VMMCreds)
	// The console log is opened by noded as root and inherited as an fd, so the
	// dropped uid writes to it without needing to open it. The fd is already open
	// at the point of the drop, which is what makes that work.
	if err := r.VMMCreds.chown(logFile.Name()); err != nil {
		return fmt.Errorf("fc: %w", err)
	}
	// The working directory is the sandbox's own, which is what makes the vsock
	// UDS path relative and therefore portable across a restore: Firecracker
	// saves that path into the machine state and refuses to override it on load,
	// so a relative path is the only way a snapshot taken by one sandbox can be
	// restored into another.
	cmd.Dir = vm.dir

	// Joining the namespace does not disturb any of the above. The cwd is
	// unaffected by setns (measured, network.md section 1), and the log fds are
	// inherited because this forks the same cmd rather than running it under a
	// helper binary.
	if err := startInNetns(cmd, vm.netnsPath); err != nil {
		return fmt.Errorf("fc: start firecracker: %w", err)
	}
	vm.cmd = cmd

	go func() {
		_ = cmd.Wait()
		close(vm.done)
	}()

	// The process exists now, so a failure below leaves a running VMM. It is not
	// killed here: the caller's cleanup stack already holds killVMM, and vm.cmd is
	// set above so that stack can reach it.
	pid := cmd.Process.Pid
	if err := vm.cgroup.Add(pid); err != nil {
		return fmt.Errorf("fc: %w", err)
	}
	if err := applyRlimits(pid, r.VMMCreds); err != nil {
		return fmt.Errorf("fc: %w", err)
	}
	return nil
}

// chownRootfsDevice hands the sandbox's block device to the dropped uid.
//
// Separate from the sandbox directory walk because rootfs.img is a symlink to
// /dev/mapper/bean-<id> and the walk does not follow symlinks -- deliberately, so
// it cannot chown a shared asset or a device node by accident. This chowns the
// device node itself, by resolving the link.
//
// The device is per-sandbox, created and destroyed with it, so giving it to the
// sandbox uid takes nothing away from anything else on the host. That is what
// separates it from /dev/kvm, which is shared and reached through its group
// instead.
//
// A dropped uid that cannot open this fails at boot: the guest kernel finds no
// root device, and the only evidence is in the console log.
func (r *FCRuntime) chownRootfsDevice(rootfs *image.Rootfs) error {
	if !r.VMMCreds.Enabled() || rootfs == nil || rootfs.Device == "" {
		return nil
	}
	// EvalSymlinks rather than Readlink: the FileProvider hands back a real file
	// with no link at all, and that case must be chowned too.
	target, err := filepath.EvalSymlinks(rootfs.Device)
	if err != nil {
		return fmt.Errorf("fc: resolve rootfs %s: %w", rootfs.Device, err)
	}
	if err := r.VMMCreds.chown(target); err != nil {
		return fmt.Errorf("fc: hand rootfs device to the VMM uid: %w", err)
	}
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
		TrackDirtyPages: r.TrackDirtyPages,
	}); err != nil {
		return err
	}
	// Recorded on the VM because a checkpoint cannot recover it later: the
	// setting lives in KVM, not in anything the VM reports. A diff requested
	// against a guest booted without it has to fail rather than quietly produce a
	// full snapshot, which would look like a saving that did not happen.
	vm.dirtyPages = r.TrackDirtyPages

	// Masking has to happen before InstanceStart. A guest reads CPUID once
	// during early boot and caches what it found — glibc picks its string
	// routines from it — so a template applied any later would be masking
	// features the guest has already committed to using.
	if cfg := cpuConfigFor(r.CPUTemplate); cfg != nil {
		if err := vm.client.put(ctx, "/cpu-config", cfg); err != nil {
			return fmt.Errorf("apply cpu template %s: %w", r.CPUTemplate, err)
		}
	}

	// The agent disk boots as the root device and the user image is attached
	// beside it. The kernel execs init from whatever it mounted as root, so
	// putting the agent there is what keeps user images free of any obligation
	// to embed beand or an init system: the agent pivots to the user rootfs
	// once it is running.
	//
	// Panic reboots are disabled so a crashed guest stays inspectable rather
	// than looping.
	//
	// A console is attached even in the default configuration, at a loglevel that
	// carries errors and nothing else.
	//
	// "quiet" alone was the earlier choice, to avoid the 8250 UART's synchronous
	// writes stalling the boot -- every line the kernel emits costs time. But it
	// attaches no console device at all, so a guest that dies during boot writes its
	// reason nowhere. Measured: a failing boot produced zero explanatory lines under
	// quiet and the cause under console=ttyS0, while noded reported only "agent not
	// healthy after 20s" either way. The console log then held Firecracker's own
	// output, which describes the VMM's reaction rather than the guest's failure and
	// reads like a hardware fault -- misleading in a way that silence is not.
	//
	// loglevel=3 is KERN_ERR and above, which excludes the several hundred lines of
	// initialisation that made a full console expensive while keeping panics, mount
	// failures and anything init prints on its way out. Measured at 1108-1119ms for a
	// cold create before this change; the check below re-measures it.
	console := "console=ttyS0 loglevel=3"
	if r.DebugConsole {
		// Everything, including the initialisation chatter, for the case where a
		// guest fails before it reaches the point of logging an error.
		console = "console=ttyS0"
	}
	bootArgs := fmt.Sprintf(
		"%s reboot=k panic=-1 pci=off%s init=/bean/beand -- --listen vsock:%d --pivot %s%s",
		console, guestIPBootArg(spec.Network), agentVsockPort, guestRootfsDevice,
		GuestDNSBootArgs(r.GuestDNS))
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

	// The interface has to exist before InstanceStart, the same constraint the
	// CPU mask has: Firecracker rejects a network device on a running machine
	// (the endpoint is pre-boot only, and PATCH afterwards only swaps rate
	// limiters). A guest started without one has no NIC for the rest of its life,
	// and the symptom is not an error anywhere on this path -- it is pip and git
	// failing inside the sandbox much later.
	//
	// A nil layout means this node has no networking configured, which stays the
	// pre-existing behaviour of no interface at all rather than a boot failure.
	if spec.Network != nil {
		if err := vm.client.put(ctx, "/network-interfaces/"+guestIfaceID,
			fcNetworkInterface{
				IfaceID: guestIfaceID,
				// Not made relative like the drives and the vsock UDS. This is a
				// device name resolved in the VMM's network namespace rather than a
				// path resolved from its working directory, and it is portable for a
				// different reason: the name is identical in every namespace, so a
				// snapshot finds the device it recorded without an override.
				HostDevName: spec.Network.TapName,
			}); err != nil {
			return fmt.Errorf("register network interface on tap %s: %w",
				spec.Network.TapName, err)
		}

		// Bound to this one interface, and configured here because the binding is
		// pre-boot state that a snapshot carries -- unlike the metadata contents,
		// which are written per restore. Splitting them is what lets a restored
		// guest be handed a fresh token without reconfiguring a booted machine.
		//
		// Inside the sandbox's namespace 169.254.169.254 is Firecracker's own
		// service, not a cloud provider's: the guest has no route off its /30
		// except through the tap, and the host filter drops the metadata range
		// (see network/rules.go), so this address cannot reach anything else.
		if err := vm.client.put(ctx, "/mmds/config", fcMmdsConfig{
			Version:           "V2",
			NetworkInterfaces: []string{guestIfaceID},
		}); err != nil {
			return fmt.Errorf("configure the metadata service: %w", err)
		}
	}

	// Written before the machine starts so the agent can read it during its own
	// startup. The agent gates every request on this value, so a guest that began
	// running before it existed would answer requests it cannot yet authenticate.
	if spec.AgentTokenHash != "" {
		if err := vm.client.put(ctx, "/mmds",
			fcMmds{AgentTokenHash: spec.AgentTokenHash}); err != nil {
			return fmt.Errorf("publish the agent token hash: %w", err)
		}
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
