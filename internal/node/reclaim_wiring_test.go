package node

import (
	"context"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/reclaim"
)

// recordingHost captures what reconciliation saw, so a test can assert on the
// expected set that reached it rather than on the reclamation logic, which is
// tested in internal/node/reclaim.
type recordingHost struct {
	mu       sync.Mutex
	passes   int
	removed  []string
	mappings []string
}

func (h *recordingHost) ListDMNames() ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.passes++
	return h.mappings, nil
}

func (h *recordingHost) RemoveDM(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, name)
	return nil
}

func (h *recordingHost) ListLoopDevices() ([]reclaim.LoopDevice, error) { return nil, nil }
func (h *recordingHost) DetachLoop(string) error                        { return nil }
func (h *recordingHost) ListSandboxDirs() ([]string, error)             { return nil, nil }
func (h *recordingHost) RemoveSandboxDir(string) error                  { return nil }

func (h *recordingHost) snapshot() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.passes, append([]string(nil), h.removed...)
}

// TestReclaimUsesControlPlaneExpectedSet is the wiring the safety of the whole
// feature depends on. A sandbox the control plane still expects has no record in
// this process after a restart, so if the expected set does not reach the
// reconciler its mapping looks exactly like garbage.
func TestReclaimUsesControlPlaneExpectedSet(t *testing.T) {
	mgr := newTestManager(t)
	host := &recordingHost{mappings: []string{"bean-sbx_live", "bean-sbx_dead"}}

	// The control plane expects sbx_live. Neither sandbox exists in this manager,
	// which is the post-crash state: the host holds both, memory holds neither.
	cp := newFakeCP([]*nodev1.SandboxSpec{{SandboxId: "sbx_live"}})
	addr := startFakeCP(t, cp)

	reg := testRegistrar(mgr, addr)
	reg.ReclaimHost = host
	reg.BaseDir = t.TempDir()
	reg.ImageDir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reg.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, removed := host.snapshot(); len(removed) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconciliation never removed the orphaned mapping")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, removed := host.snapshot()
	if len(removed) != 1 || removed[0] != "bean-sbx_dead" {
		t.Errorf("removed = %v, want only bean-sbx_dead: the expected set did not "+
			"protect the running sandbox", removed)
	}
}

// TestReclaimRunsOnceAcrossSessions: the registrar re-registers and re-syncs
// whenever the control-plane connection drops, but a reconnect says nothing about
// host state. Repeating the pass would race the sandboxes this process has created
// since startup, whose mappings exist on the host and whose ids the control plane
// may not yet have acknowledged.
func TestReclaimRunsOnceAcrossSessions(t *testing.T) {
	mgr := newTestManager(t)
	host := &recordingHost{}
	cp := newFakeCP(nil)
	addr := startFakeCP(t, cp)

	reg := testRegistrar(mgr, addr)
	reg.ReclaimHost = host
	reg.BaseDir = t.TempDir()
	reg.ImageDir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reg.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if passes, _ := host.snapshot(); passes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconciliation never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Drive a second reconcile directly: it is what a reconnect does, without the
	// timing of an actual connection drop.
	reg.reclaimHost(map[string]bool{})
	reg.reclaimHost(map[string]bool{})
	if passes, _ := host.snapshot(); passes != 1 {
		t.Errorf("reconciliation passes = %d, want 1", passes)
	}
}

// TestReclaimDisabledWithoutHost keeps the local runtime and single-node mode
// clear of this entirely: there is no device-mapper state to reconcile, and a nil
// host must be a no-op rather than a nil dereference on every startup.
func TestReclaimDisabledWithoutHost(t *testing.T) {
	mgr := newTestManager(t)
	reg := testRegistrar(mgr, "127.0.0.1:0")
	reg.reclaimHost(map[string]bool{"sbx_a": true})
}
