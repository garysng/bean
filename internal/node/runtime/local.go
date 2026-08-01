package runtime

import (
	"context"
	"fmt"
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

func (r *LocalRuntime) Create(ctx context.Context, spec *Spec) (*Handle, error) {
	root := filepath.Join(r.baseDir, spec.SandboxID, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	// Unix socket paths are limited to ~104 bytes on darwin; use a short tmp dir.
	sockDir, err := os.MkdirTemp("", "bn")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	sock := filepath.Join(sockDir, "a.sock")

	cmd := exec.Command(r.agentBin, "--listen", sock, "--root", root)
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

func (r *LocalRuntime) List(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.procs))
	for id := range r.procs {
		ids = append(ids, id)
	}
	return ids, nil
}
