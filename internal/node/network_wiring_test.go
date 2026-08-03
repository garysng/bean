package node

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/garysng/bean/internal/node/network"
	"github.com/garysng/bean/internal/node/runtime"
)

// These tests are about the manager's ordering obligations, not about namespaces.
// What leaks host resources when it is wrong is the sequence -- release the slot
// when setup fails, tear down before the record goes, do neither when networking
// is off -- and all of it is observable through a fake, on any platform. The
// commands themselves are covered in internal/node/network.

// fakeProvisioner is a Provisioner that records calls, backed by a real
// address pool so an index handed out twice is visible rather than assumed
// impossible.
type fakeProvisioner struct {
	mu         sync.Mutex
	live       map[string]bool
	provisions []string
	releases   []string
	nextIndex  int
	failWith   error
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{live: map[string]bool{}}
}

func (f *fakeProvisioner) Provision(id string) (*network.Layout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.provisions = append(f.provisions, id)
	l, err := network.LayoutFor(f.nextIndex, "172.31.0.0/30")
	if err != nil {
		return nil, err
	}
	f.nextIndex++
	f.live[id] = true
	return l, nil
}

func (f *fakeProvisioner) Deprovision(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, id)
	delete(f.live, id)
	return nil
}

func (f *fakeProvisioner) held() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.live))
	for id := range f.live {
		out = append(out, id)
	}
	return out
}

func (f *fakeProvisioner) snapshot() (prov, rel []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.provisions...), append([]string(nil), f.releases...)
}

func newNetworkedManager(t *testing.T) (*Manager, *fakeProvisioner) {
	t.Helper()
	m := NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	net := newFakeProvisioner()
	m.Net = net
	t.Cleanup(m.Close)
	return m, net
}

// TestCreateProvisionsNetworkingAndDestroyReleasesIt is the pair. The release
// assertion is the one that matters: a destroy that does not tear down leaves the
// namespace on the host, its index looks occupied to the allocator forever, and
// the node's capacity falls by one per sandbox destroyed. That is the loop-device
// leak (GitHub #16) in a resource whose reuse also collides addresses.
func TestCreateProvisionsNetworkingAndDestroyReleasesIt(t *testing.T) {
	m, net := newNetworkedManager(t)
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("net-1")); err != nil {
		t.Fatal(err)
	}
	prov, _ := net.snapshot()
	if len(prov) != 1 || prov[0] != "net-1" {
		t.Fatalf("provisioned %v, want [net-1]", prov)
	}

	if err := m.Destroy(ctx, "net-1", false); err != nil {
		t.Fatal(err)
	}
	_, rel := net.snapshot()
	if len(rel) != 1 || rel[0] != "net-1" {
		t.Fatalf("released %v after destroy, want [net-1]; an index that is never "+
			"released is an address this node can never use again", rel)
	}
	if held := net.held(); len(held) != 0 {
		t.Errorf("still holding %v after destroy", held)
	}
}

// TestCreateFailsWhenNetworkSetupFails is the other half of the ordering. A
// sandbox that boots believing it has a network, with nothing attached, is the
// failure docs/network.md section 7 says makes people doubt their own code: the
// guest comes up, pip fails, and nothing on the create path reported an error.
//
// Firecracker's network-interface endpoint is pre-boot only, so this cannot be
// repaired after the fact either -- the guest has no NIC for the rest of its life.
func TestCreateFailsWhenNetworkSetupFails(t *testing.T) {
	m, net := newNetworkedManager(t)
	net.failWith = errors.New("synthetic setup failure")
	ctx := context.Background()

	sb, err := m.Create(ctx, spec("net-doomed"))
	if err == nil {
		t.Fatal("create succeeded despite the network setup failing; this sandbox " +
			"is running with no interface and nothing said so")
	}
	if sb != nil {
		t.Error("a sandbox was returned alongside the error")
	}
	// The record must not survive, or the node reports a sandbox nothing can reach
	// and no destroy will ever be sent for it.
	if m.Get("net-doomed") != nil {
		t.Error("failed sandbox left in the manager map")
	}
	if len(m.Statuses()) != 0 {
		t.Errorf("statuses = %v, want empty", m.Statuses())
	}
}

// TestCreateReleasesTheSlotWhenTheRuntimeFails covers the path between the two:
// networking was built, then the runtime refused. Without the release the node
// leaks a namespace per failed create, which is invisible until it starts
// refusing creates at a count nobody can account for.
func TestCreateReleasesTheSlotWhenTheRuntimeFails(t *testing.T) {
	m := NewManager(&failingRuntime{})
	net := newFakeProvisioner()
	m.Net = net
	t.Cleanup(m.Close)

	if _, err := m.Create(context.Background(), spec("net-rt-fail")); err == nil {
		t.Fatal("expected the runtime failure to fail the create")
	}
	_, rel := net.snapshot()
	if len(rel) != 1 || rel[0] != "net-rt-fail" {
		t.Fatalf("released %v, want [net-rt-fail]; a create that failed after "+
			"networking was built must give the namespace back", rel)
	}
	if held := net.held(); len(held) != 0 {
		t.Errorf("still holding %v after a failed create", held)
	}
}

// TestNoProvisionerLeavesSandboxesExactlyAsTheyWere is the deployment with
// networking unconfigured, and it is not an edge case: it is every node until an
// operator sets the flags. Such a node must create and destroy sandboxes with no
// namespace, no NIC and no rules -- exactly the behaviour before this existed.
func TestNoProvisionerLeavesSandboxesExactlyAsTheyWere(t *testing.T) {
	m := newTestManager(t)
	if m.Net != nil {
		t.Fatal("a manager with no networking configured must have a nil provisioner")
	}
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("no-net")); err != nil {
		t.Fatalf("create failed on a node with no networking configured: %v", err)
	}
	if _, rel, err := m.AgentConn(ctx, "no-net"); err != nil {
		t.Fatalf("agent unreachable without networking: %v", err)
	} else {
		rel()
	}
	if err := m.Destroy(ctx, "no-net", false); err != nil {
		t.Fatalf("destroy failed on a node with no networking configured: %v", err)
	}
	if m.Get("no-net") != nil {
		t.Error("sandbox left in the manager map")
	}
}

// TestSpecCarriesNoLayoutWithoutAProvisioner checks the value the runtime tier
// actually branches on. fc_linux.go registers a NIC only when spec.Network is
// non-nil, so a zero-valued layout rather than a nil one would make an
// unconfigured node try to attach to a tap that was never created.
func TestSpecCarriesNoLayoutWithoutAProvisioner(t *testing.T) {
	rt := &specCapturingRuntime{}
	m := NewManager(rt)
	t.Cleanup(m.Close)

	_, _ = m.Create(context.Background(), spec("spec-no-net"))
	if got := rt.seen(); got == nil {
		t.Fatal("runtime never saw a spec")
	} else if got.Network != nil {
		t.Errorf("spec.Network = %+v on a node with no networking configured, want "+
			"nil; the fc tier reads this to decide whether to attach a NIC", got.Network)
	}
}

// TestSpecCarriesTheLayoutWhenConfigured is the same assertion in the other
// direction. Provisioning a namespace and then not telling the runtime about it
// would leave the sandbox with a tap nothing is attached to, which is silent.
func TestSpecCarriesTheLayoutWhenConfigured(t *testing.T) {
	rt := &specCapturingRuntime{}
	m := NewManager(rt)
	m.Net = newFakeProvisioner()
	t.Cleanup(m.Close)

	_, _ = m.Create(context.Background(), spec("spec-net"))
	got := rt.seen()
	if got == nil {
		t.Fatal("runtime never saw a spec")
	}
	if got.Network == nil {
		t.Fatal("spec.Network is nil although a namespace was provisioned; the tap " +
			"exists and no guest is attached to it")
	}
	if got.Network.TapName != "beantap0" {
		t.Errorf("tap = %q, want beantap0; the constant name is what lets a restored "+
			"snapshot find its device", got.Network.TapName)
	}
}

// specCapturingRuntime records the spec it was handed and then fails, so the
// assertion is about what the manager passed down rather than about booting.
type specCapturingRuntime struct {
	failingRuntime
	mu   sync.Mutex
	spec *runtime.Spec
}

func (s *specCapturingRuntime) Create(_ context.Context, spec *runtime.Spec) (*runtime.Handle, error) {
	s.mu.Lock()
	s.spec = spec
	s.mu.Unlock()
	return nil, errors.New("synthetic create failure after the spec was captured")
}

func (s *specCapturingRuntime) seen() *runtime.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec
}

// TestCloseTearsDownNetworkingForRunningSandboxes: a clean shutdown must not
// leave a namespace per running sandbox for the next process to adopt. Those
// indices would be unavailable to the restarted node, which is a capacity loss
// that compounds across restarts.
func TestCloseTearsDownNetworkingForRunningSandboxes(t *testing.T) {
	m := NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	net := newFakeProvisioner()
	m.Net = net
	ctx := context.Background()

	for _, id := range []string{"shut-1", "shut-2"} {
		if _, err := m.Create(ctx, spec(id)); err != nil {
			t.Fatal(err)
		}
	}
	m.Close()

	if held := net.held(); len(held) != 0 {
		t.Errorf("namespaces %v survived shutdown; the next noded cannot use those "+
			"indices and the node's capacity drops with every restart", held)
	}
}
