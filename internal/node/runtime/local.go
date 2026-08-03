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
)

// LocalRuntime runs each sandbox as a host process tree: the real
// beand binary confined to a per-sandbox root dir. It is the
// dev/CI runtime (darwin/linux, no KVM needed) and exercises the exact
// same agent gRPC surface as the fc tier.
type LocalRuntime struct {
	agentBin string
	baseDir  string

	// GuestDNS is the resolver the agent writes into the sandbox's
	// /etc/resolv.conf. Empty leaves the image's own file alone, which is what a
	// node with no sandbox networking wants.
	GuestDNS string

	mu    sync.Mutex
	procs map[string]*localSandbox
}

type localSandbox struct {
	cmd    *exec.Cmd
	root   string
	sock   string
	paused bool
	done   chan struct{} // closed when the agent process exits
}

func NewLocalRuntime(agentBin, baseDir string) *LocalRuntime {
	return &LocalRuntime{agentBin: agentBin, baseDir: baseDir, procs: map[string]*localSandbox{}}
}

func (r *LocalRuntime) Name() string { return "local" }

// agentArgs assembles the agent's command line for one sandbox.
//
// --guest-dns is omitted rather than passed empty when no resolver is
// configured, so a node without networking spawns the agent with exactly the
// arguments it used before this flag existed. "Unset" then means the previous
// behaviour rather than a new path that has to be trusted separately.
func (r *LocalRuntime) agentArgs(sock, root string) []string {
	args := []string{"--listen", sock, "--root", root}
	if r.GuestDNS != "" {
		args = append(args, "--guest-dns", r.GuestDNS)
	}
	return args
}

func (r *LocalRuntime) Create(ctx context.Context, spec *Spec) (*Handle, error) {
	return r.create(ctx, spec, nil)
}

// Checkpoint archives the sandbox rootfs. Process state is not captured:
// LocalRuntime exists for development and CI, where filesystem fidelity is
// what the control-plane paths under test actually depend on.
// Checkpoint archives the sandbox's filesystem.
//
// The options are ignored because this tier has no guest memory to capture: a
// local sandbox is a process on the host, and its checkpoint is a directory
// either way. So a restore here always starts the process fresh, which is what
// IncludeMemory=false means on the microVM tier — the difference the option
// describes does not exist here rather than being unimplemented.
func (r *LocalRuntime) Checkpoint(ctx context.Context, id string, w io.Writer, _ CheckpointOptions) error {
	sb, err := r.get(id)
	if err != nil {
		return err
	}
	return tarDirectory(sb.root, w)
}

// Fork creates a sandbox and unpacks a checkpoint over its rootfs.
//
// This tier has no memory state, so a chain has nothing to layer: each layer is a
// tar of the whole filesystem, and the leaf already holds everything its
// ancestors did. Earlier layers are drained and discarded, which keeps the sender
// from blocking on a stream nobody reads.
//
// Each sandbox unpacks into its own directory, so siblings from one checkpoint
// are independent here for the same reason they are on the microVM tier, just
// by copying rather than by copy-on-write.
func (r *LocalRuntime) Fork(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error) {
	if len(layers) == 0 {
		return nil, errors.New("local: fork needs at least one snapshot layer")
	}
	for _, layer := range layers[:len(layers)-1] {
		if _, err := io.Copy(io.Discard, layer.Data); err != nil {
			return nil, fmt.Errorf("local: drain layer %s: %w", layer.ID, err)
		}
	}
	return r.create(ctx, spec, layers[len(layers)-1].Data)
}

// create starts a sandbox, optionally seeding its rootfs from a checkpoint
// before the agent comes up so the agent never observes a partial rootfs.
func (r *LocalRuntime) create(ctx context.Context, spec *Spec, restoreFrom io.Reader) (*Handle, error) {
	root := filepath.Join(r.baseDir, spec.SandboxID, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	if restoreFrom != nil {
		if err := untarDirectory(restoreFrom, root); err != nil {
			_ = os.RemoveAll(filepath.Join(r.baseDir, spec.SandboxID))
			return nil, fmt.Errorf("restore rootfs: %w", err)
		}
	}
	// Unix socket paths are limited to ~104 bytes on darwin; use a short tmp dir.
	sockDir, err := os.MkdirTemp("", "bn")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	sock := filepath.Join(sockDir, "a.sock")

	cmd := exec.Command(r.agentBin, r.agentArgs(sock, root)...)
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	// cleanupFailed tears down everything created above.
	cleanupFailed := func() {
		_ = signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		_ = os.RemoveAll(sockDir)
		_ = os.RemoveAll(filepath.Join(r.baseDir, spec.SandboxID))
	}

	// Wait for the agent socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cleanupFailed()
			return nil, fmt.Errorf("agent socket not ready: %s", sock)
		}
		select {
		case <-ctx.Done():
			cleanupFailed()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}

	r.mu.Lock()
	r.procs[spec.SandboxID] = &localSandbox{cmd: cmd, root: root, sock: sock, done: done}
	r.mu.Unlock()

	return &Handle{
		SandboxID:  spec.SandboxID,
		AgentAddr:  "unix://" + sock,
		StartedAt:  time.Now(),
		PID:        cmd.Process.Pid,
		RuntimeTag: r.Name(),
	}, nil
}

func (r *LocalRuntime) get(id string) (*localSandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.procs[id]
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found in runtime", id)
	}
	return sb, nil
}

func (r *LocalRuntime) Destroy(ctx context.Context, id string, force bool) error {
	sb, err := r.get(id)
	if err != nil {
		return nil // idempotent
	}
	// A paused sandbox is stopped with SIGSTOP, and a stopped process does not
	// act on SIGTERM — it stays queued until the process runs again. Without the
	// SIGCONT first, destroying a paused sandbox always waited out the full
	// grace period and then killed it, which is the same blind wait that made
	// destroy slow on the microVM tier.
	r.mu.Lock()
	paused := sb.paused
	r.mu.Unlock()
	if paused {
		_ = signalGroup(sb.cmd.Process.Pid, syscall.SIGCONT)
	}

	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = signalGroup(sb.cmd.Process.Pid, sig)
	if !force {
		select {
		case <-sb.done:
		case <-time.After(3 * time.Second):
			_ = signalGroup(sb.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	r.mu.Lock()
	delete(r.procs, id)
	r.mu.Unlock()
	_ = os.RemoveAll(filepath.Dir(sb.sock))
	return os.RemoveAll(filepath.Join(r.baseDir, id))
}

func (r *LocalRuntime) Pause(ctx context.Context, id string) error {
	sb, err := r.get(id)
	if err != nil {
		return err
	}
	if err := signalGroup(sb.cmd.Process.Pid, syscall.SIGSTOP); err != nil {
		return err
	}
	r.mu.Lock()
	sb.paused = true
	r.mu.Unlock()
	return nil
}

func (r *LocalRuntime) Resume(ctx context.Context, id string) error {
	sb, err := r.get(id)
	if err != nil {
		return err
	}
	if err := signalGroup(sb.cmd.Process.Pid, syscall.SIGCONT); err != nil {
		return err
	}
	r.mu.Lock()
	sb.paused = false
	r.mu.Unlock()
	return nil
}
