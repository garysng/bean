//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"

	"log/slog"
)

// OCIRuntime runs each sandbox as a container through an OCI runtime binary --
// runsc (gVisor) by default, or runc.
//
// It sits between the two tiers that already exist. The local tier runs the agent as
// an ordinary host process with no isolation; the fc tier boots a microVM with its own
// kernel. This one gives a sandbox its own namespaces, cgroup and filesystem while
// staying a process on the host kernel -- or, under gVisor, on a userspace kernel that
// intercepts its syscalls.
//
// # Why the agent is reached over TCP rather than a Unix socket
//
// The local tier uses a Unix socket, and doing the same here does not work under
// gVisor: a process inside binds the socket successfully and the host never sees it,
// because gVisor implements Unix sockets inside the Sentry rather than as nodes on the
// host filesystem. Measured with hack/gvisor-probe.sh -- an ordinary file written to
// the same bind mount *does* appear on the host, so this is specific to sockets.
//
// So the agent listens on TCP inside the sandbox's network namespace and the node
// dials in through it, using the `netns:<path>|<host>:<port>` transport that
// internal/node/dial.go already implements and internal/node/portforward.go already
// uses for proxied ports. Confirmed reachable through a real gVisor sandbox with
// hack/gvisor-netns-probe.sh.
//
// The consequence is that this tier needs node networking. Without a namespace there
// is no address to dial, and Create says so rather than starting a sandbox nothing can
// reach.
type OCIRuntime struct {
	// Bin is the OCI runtime binary: "runsc" or "runc".
	//
	// The two differ in isolation, not in interface -- both take a bundle and the same
	// subcommands -- so which one a node uses is configuration rather than a second
	// implementation. gVisor is the stronger choice and the default; runc is what a
	// node needs for GPU work, which gVisor does not support.
	Bin string
	// ExtraArgs go before the subcommand, for flags the binary needs globally.
	ExtraArgs []string
	// AgentBin is the agent binary on the host, copied into the sandbox's rootfs.
	//
	// Copied rather than bind-mounted: a bind mount would make the running node's
	// binary part of every sandbox's filesystem, so replacing it during an upgrade
	// would change what already-running sandboxes are executing.
	AgentBin string
	// BaseDir holds one directory per sandbox: the bundle, the mounted rootfs, and
	// whatever the agent writes.
	BaseDir string
	// Images provides the rootfs.
	Images image.Provider
	// AgentPort is where the agent listens inside the namespace.
	//
	// The same port in every sandbox, which is safe twice over: each sandbox has its
	// own network namespace, and the node dials a per-sandbox veth address inside it.
	// So neither the port nor the address collides.
	AgentPort int
	// GuestDNS is the resolver written into the sandbox's /etc/resolv.conf.
	GuestDNS string

	// agentPath is AgentBin resolved to a real path by Available. Held because the
	// copy into a sandbox's rootfs opens the file, and a bare name that only PATH can
	// resolve is not something os.ReadFile can open.
	agentPath string

	mu        sync.Mutex
	sandboxes map[string]*ociSandbox
}

type ociSandbox struct {
	id string
	// dir is the per-sandbox directory: bundle, rootfs mount point, agent dir.
	dir string
	// releaseRootfs unmounts the rootfs and frees the device, in that order.
	releaseRootfs func() error
	// stopMMDS shuts down the per-sandbox metadata service.
	stopMMDS  func() error
	agentAddr string
	startedAt time.Time
	paused    bool
}

const defaultAgentPort = 8111

func NewOCIRuntime(bin, agentBin, baseDir string, images image.Provider) *OCIRuntime {
	return &OCIRuntime{
		Bin:       bin,
		AgentBin:  agentBin,
		BaseDir:   baseDir,
		Images:    images,
		AgentPort: defaultAgentPort,
		sandboxes: map[string]*ociSandbox{},
	}
}

func (r *OCIRuntime) Name() string { return filepath.Base(r.Bin) }

// Available reports whether this host can run the tier, so a node fails to start
// rather than accepting placements it cannot honour -- the same reasoning as
// OverlaybdProvider.Available.
func (r *OCIRuntime) Available() error {
	if r.Bin == "" {
		return errors.New("runtime: no OCI runtime binary configured")
	}
	path, err := exec.LookPath(r.Bin)
	if err != nil {
		return fmt.Errorf("runtime: %s not found: %w", r.Bin, err)
	}
	// Version rather than presence: a binary that cannot run at all -- wrong
	// architecture, missing loader -- would otherwise fail at the first create.
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("runtime: %s --version: %w: %s", r.Bin, err,
			strings.TrimSpace(string(out)))
	}
	if r.AgentBin == "" {
		return errors.New("runtime: no agent binary configured")
	}
	// Resolved through PATH, not stat'ed directly: --agent-bin defaults to the bare
	// name "beand", which is what the local tier passes to exec and which resolves
	// fine there. Stat'ing it rejected a node whose agent was simply on PATH.
	//
	// The resolved path is kept, because the copy into each sandbox's rootfs reads the
	// file rather than executing it -- and a name that only PATH can resolve is not a
	// file anything can open.
	resolved, err := exec.LookPath(r.AgentBin)
	if err != nil {
		return fmt.Errorf("runtime: agent binary %s: %w", r.AgentBin, err)
	}
	r.agentPath = resolved
	return nil
}

func (r *OCIRuntime) Create(ctx context.Context, spec *Spec) (h *Handle, err error) {
	if spec == nil || spec.SandboxID == "" {
		return nil, errors.New("runtime: sandbox id required")
	}
	// Checked before any work: without a namespace the node has no address to reach
	// the agent on, so the sandbox would start and be unusable. Better to refuse than
	// to leave a running container nothing can talk to.
	netnsPath := netnsPathFor(spec)
	if netnsPath == "" {
		return nil, errors.New("runtime: the container tier needs node networking " +
			"(--guest-subnet); the agent is reached through the sandbox's network namespace")
	}
	// The veth address, not GuestIP.
	//
	// GuestIP is what a microVM's guest kernel configures on the tap device, and a
	// container has no guest kernel: the tap stays DOWN and that address exists
	// nowhere. Measured -- the namespace held 172.31.0.1/30 on a DOWN beantap0 and
	// 10.0.0.2/30 on an UP veth, while the node dialled 172.31.0.2 and got "network is
	// unreachable" because the host resolved it through the default gateway.
	//
	// A container process runs in the namespace directly, so the address that reaches
	// it is the namespace end of the veth pair the node built.
	if spec.Network.NetnsLinkIP == nil {
		return nil, errors.New("runtime: sandbox network has no namespace address; " +
			"the container tier is reached through the veth, not the tap")
	}
	// Through SandboxIP rather than reading the field again, so the agent's address
	// and the one port forwarding dials cannot drift apart.
	agentIP := r.SandboxIP(spec.Network)

	dir := filepath.Join(r.BaseDir, spec.SandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime: create sandbox dir: %w", err)
	}

	var cleanup []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	cleanup = append(cleanup, func() { _ = os.RemoveAll(dir) })

	rootfs, err := r.Images.Prepare(ctx, spec.SandboxID, spec.Image, image.PrepareOptions{
		SizeMiB: spec.DiskMiB,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: prepare rootfs: %w", err)
	}
	// From here the rootfs is owned by releaseRootfs, which supersedes
	// rootfs.Release: unmounting has to happen before the device is detached.
	rootfsDir, releaseRootfs, err := image.MountDir(rootfs, dir)
	if err != nil {
		_ = rootfs.Release()
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = releaseRootfs() })

	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime: create agent dir: %w", err)
	}

	// The agent goes into the rootfs rather than being bind-mounted, so a node binary
	// replaced during an upgrade does not change what running sandboxes execute.
	agentInGuest := "/.bean/beand"
	agentSrc := r.agentPath
	if agentSrc == "" {
		// Available normally fills this in. A runtime constructed directly in a test
		// has not been through it, so fall back rather than fail on a nil field.
		agentSrc = r.AgentBin
	}
	if err := copyAgent(agentSrc, filepath.Join(rootfsDir, agentInGuest)); err != nil {
		return nil, err
	}

	if r.GuestDNS != "" {
		// Written directly rather than passed to the agent: the agent's --guest-dns
		// rewrites resolv.conf after pivoting, and there is no pivot here.
		if err := writeResolvConf(rootfsDir, r.GuestDNS); err != nil {
			// Not fatal, for the same reason the fc tier only warns: a sandbox with no
			// resolver still runs, and refusing the create trades a working sandbox for
			// a configuration detail.
			slog.Warn("could not write resolv.conf into the sandbox",
				logging.KeySandbox, spec.SandboxID, logging.KeyError, err)
		}
	}

	if err := writeBundleConfig(dir, bundleConfig{
		RootfsDir: rootfsDir,
		// tcp: is required, not optional. The agent accepts vsock:PORT, tcp:HOST:PORT
		// or an absolute Unix socket path, and treats anything else as a socket path --
		// so a bare "0.0.0.0:8111" was rejected as a relative path, the agent exited,
		// and the container was `stopped` before the first health check.
		Args: []string{agentInGuest,
			"--listen", fmt.Sprintf("tcp:0.0.0.0:%d", r.AgentPort),
			"--root", "/"},
		Env:         envList(spec.Env),
		NetnsPath:   netnsPath,
		AgentDir:    agentDir,
		MemoryMiB:   spec.MemoryMiB,
		CPU:         spec.CPU,
		CgroupsPath: "",
	}); err != nil {
		return nil, err
	}

	// The metadata service comes up before the container, because the agent reads it
	// during its first handshake: a container started first would have its earliest
	// calls denied with "agent cannot determine its expected credential", which the
	// node's dial treats as an agent that is not ready -- accurate but slow, and
	// indistinguishable from a sandbox that never comes up.
	//
	// AgentTokenHash is what the Manager published for this sandbox. Required here
	// rather than optional: the agent demands a token on a TCP listener, so a sandbox
	// with no hash is one nothing can talk to.
	if spec.AgentTokenHash == "" {
		return nil, errors.New("runtime: the container tier needs an agent token; its " +
			"TCP transport is reachable from inside the sandbox, so the agent requires one")
	}
	stopMMDS, err := startMMDS(netnsPath, spec.Network.NetnsVeth, spec.AgentTokenHash)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = stopMMDS() })

	// The entry process's own output goes to a file rather than being discarded.
	//
	// --detach returns as soon as the container is created, so a `runsc run` that
	// succeeds says nothing about whether the process inside survived. Without this the
	// agent failing to start was invisible: the container went straight to `stopped`
	// with pid -1, and the only symptom reaching the caller was a health-check timeout
	// on an address whose namespace was empty -- which reads as a network fault.
	//
	// Named console.log rather than kept in memory because the process outlives this
	// call, and BootLogTail reads it back when a create fails.
	consolePath := filepath.Join(dir, "console.log")
	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtime: create console log: %w", err)
	}
	defer console.Close()

	// --detach keeps the container running past this call. Without it the call blocks
	// for the sandbox's whole life.
	cmd := r.command(ctx, "run", "--detach", "--bundle", dir, spec.SandboxID)
	cmd.Stdout = console
	cmd.Stderr = console
	if err := cmd.Run(); err != nil {
		tail, _ := os.ReadFile(consolePath)
		return nil, fmt.Errorf("runtime: %s run: %w: %s", r.Bin, err,
			strings.TrimSpace(string(tail)))
	}
	cleanup = append(cleanup, func() {
		_ = r.command(context.WithoutCancel(ctx), "delete", "--force", spec.SandboxID).Run()
	})

	// netns: because the address only exists inside this sandbox's namespace -- the
	// host:port half is identical for every sandbox on the node, so dialling it from
	// the host namespace would reach nothing, or another sandbox.
	agentAddr := fmt.Sprintf("netns:%s|%s:%d", netnsPath, agentIP, r.AgentPort)

	sb := &ociSandbox{
		id:            spec.SandboxID,
		dir:           dir,
		releaseRootfs: releaseRootfs,
		stopMMDS:      stopMMDS,
		agentAddr:     agentAddr,
		startedAt:     time.Now(),
	}
	r.mu.Lock()
	r.sandboxes[spec.SandboxID] = sb
	r.mu.Unlock()

	// Everything from here is recorded, so the cleanup chain must not run.
	cleanup = nil

	return &Handle{
		SandboxID:  spec.SandboxID,
		AgentAddr:  agentAddr,
		StartedAt:  sb.startedAt,
		RuntimeTag: r.Name(),
	}, nil
}

func (r *OCIRuntime) Destroy(ctx context.Context, id string, force bool) error {
	r.mu.Lock()
	sb := r.sandboxes[id]
	delete(r.sandboxes, id)
	r.mu.Unlock()

	var errs []error
	// --force regardless of the caller's flag when tearing down: a container that
	// ignored SIGTERM would otherwise be left behind holding its rootfs mounted, and
	// the mount is what makes a leak expensive here rather than merely untidy.
	if out, err := r.command(ctx, "delete", "--force", id).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// A container the runtime does not know about is the expected state after a
		// node restart, and is not a failure to destroy.
		if !strings.Contains(msg, "does not exist") && !strings.Contains(msg, "not found") {
			errs = append(errs, fmt.Errorf("runtime: %s delete: %w: %s", r.Bin, err, msg))
		}
	}
	if sb != nil {
		// Before the rootfs, because it is the cheaper thing to lose: a metadata service
		// left running holds a port in a namespace that is about to be torn down anyway,
		// while a rootfs left mounted holds a block device.
		if sb.stopMMDS != nil {
			if err := sb.stopMMDS(); err != nil {
				errs = append(errs, err)
			}
		}
		if err := sb.releaseRootfs(); err != nil {
			errs = append(errs, err)
		}
		if err := os.RemoveAll(sb.dir); err != nil {
			errs = append(errs, fmt.Errorf("runtime: remove sandbox dir: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Pause freezes the container's processes.
//
// Not the same thing as the fc tier's pause, and the difference matters to anything
// reasoning about cost: this stops the processes running but their memory stays
// resident, while a paused microVM can have its memory written out and reclaimed. A
// paused container still occupies its share of the node.
func (r *OCIRuntime) Pause(ctx context.Context, id string) error {
	if out, err := r.command(ctx, "pause", id).CombinedOutput(); err != nil {
		return fmt.Errorf("runtime: %s pause: %w: %s", r.Bin, err,
			strings.TrimSpace(string(out)))
	}
	r.mu.Lock()
	if sb := r.sandboxes[id]; sb != nil {
		sb.paused = true
	}
	r.mu.Unlock()
	return nil
}

func (r *OCIRuntime) Resume(ctx context.Context, id string) error {
	if out, err := r.command(ctx, "resume", id).CombinedOutput(); err != nil {
		return fmt.Errorf("runtime: %s resume: %w: %s", r.Bin, err,
			strings.TrimSpace(string(out)))
	}
	r.mu.Lock()
	if sb := r.sandboxes[id]; sb != nil {
		sb.paused = false
	}
	r.mu.Unlock()
	return nil
}

// ErrCheckpointUnsupported is returned by tiers that cannot checkpoint.
//
// A distinct error rather than a generic one because the scheduler's warm-snapshot
// path has to be able to tell "this tier cannot" from "this attempt failed": the
// first means place the work elsewhere, the second means retry.
var ErrCheckpointUnsupported = errors.New("runtime: this tier cannot checkpoint")

// Checkpoint is not implemented for containers.
//
// Doing it would mean CRIU, which is a substantial piece of work with its own
// constraints, and it would not buy what checkpointing buys on the fc tier. Warm
// snapshots are bean's main throughput lever -- a boot costs about 5 CPU-seconds
// against a restore's near-zero -- and that lever belongs to the microVM tier. This
// tier exists for what fc cannot do (GPU, no-KVM nodes), not as a substitute for it.
func (r *OCIRuntime) Checkpoint(ctx context.Context, id string, w io.Writer, opts CheckpointOptions) error {
	return fmt.Errorf("%w: %s", ErrCheckpointUnsupported, r.Name())
}

// Fork is not implemented, for the same reason as Checkpoint: there are no
// checkpoints of this tier to fork from.
func (r *OCIRuntime) Fork(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error) {
	return nil, fmt.Errorf("%w: %s", ErrCheckpointUnsupported, r.Name())
}

// command builds an invocation of the runtime binary.
//
// Global flags come before the subcommand, which is where both runc and runsc want
// them -- putting them after produces a usage error rather than a clear complaint.
func (r *OCIRuntime) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append(append([]string{}, r.ExtraArgs...), args...)
	return exec.CommandContext(ctx, r.Bin, full...)
}

// copyAgent copies the agent binary into the sandbox's rootfs.
func copyAgent(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("runtime: create agent dir in rootfs: %w", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("runtime: read agent binary: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return fmt.Errorf("runtime: install agent into rootfs: %w", err)
	}
	return nil
}

// writeResolvConf points the sandbox at a resolver.
//
// The file is replaced rather than appended to: an image ships its own, and leaving
// that in place would have the sandbox try the image's server first -- which for
// images built elsewhere is commonly an address that does not exist here.
func writeResolvConf(rootfsDir, nameserver string) error {
	path := filepath.Join(rootfsDir, "etc", "resolv.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("nameserver "+nameserver+"\n"), 0o644)
}

// envList flattens the spec's environment, adding a PATH when the caller set none.
//
// A container with no PATH cannot resolve a bare command name, which surfaces as
// "executable file not found" for a binary that is plainly there.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env)+2)
	hasPath := false
	for k, v := range env {
		if k == "PATH" {
			hasPath = true
		}
		out = append(out, k+"="+v)
	}
	if !hasPath {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}

// BootLogTail returns the last lines the sandbox's entry process wrote.
//
// Implemented for the same reason the fc tier has it, and after the same failure: the
// agent inside a container exited immediately, the container went to `stopped` with pid
// -1, and what reached the caller was "agent not healthy after 20s" on an address in an
// empty namespace. That reads as a network fault. The cause was in the process's own
// output, which --detach discarded.
//
// Read from disk rather than kept in memory because the failure path deletes the
// sandbox directory, so the Manager has to be able to ask before that happens -- which
// is why it calls this before cleaning up.
func (r *OCIRuntime) BootLogTail(id string, lines int) string {
	r.mu.Lock()
	sb := r.sandboxes[id]
	r.mu.Unlock()

	dir := ""
	if sb != nil {
		dir = sb.dir
	} else {
		// A create that failed before recording the sandbox still wrote a console log,
		// and that is precisely the case worth diagnosing.
		dir = filepath.Join(r.BaseDir, id)
	}
	data, err := os.ReadFile(filepath.Join(dir, "console.log"))
	if err != nil || len(data) == 0 {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, " | ")
}

// SandboxIP reports the veth address, which is where a container's processes are
// reachable.
//
// The tap and its GuestIP belong to a guest kernel bringing them up, and a container
// has none: the tap stays DOWN. Create dials the agent at this address for the same
// reason; this exposes it so port forwarding does not have to know which tier it is
// talking to.
func (r *OCIRuntime) SandboxIP(l *network.Layout) net.IP {
	if l == nil {
		return nil
	}
	return l.NetnsLinkIP
}
