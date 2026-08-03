package node

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/reclaim"
	"github.com/garysng/bean/internal/node/runtime"
)

// LabelAdvertiseAddr carries a node's data-plane address through
// registration labels.
const LabelAdvertiseAddr = "bean.io/advertise-addr"

// Registrar keeps a node registered with the control plane: it registers
// once, then maintains the heartbeat stream and reconciles on restart.
// The node dials out, so no inbound path to the control plane is needed.
type Registrar struct {
	ControlPlane   string
	NodeID         string
	Region         string
	Labels         map[string]string
	BootstrapToken string
	Resources      *nodev1.NodeResources
	Runtimes       []string
	// Advertise is the address the control plane should dial for this
	// node's data plane. Empty means the control plane must already know.
	Advertise string
	// ReclaimHost reclaims host resources a previous noded left behind. Nil
	// disables it, which is what the local runtime and every test that is not
	// about reconciliation want: there is nothing to reconcile without
	// device-mapper.
	ReclaimHost reclaim.Host
	// BaseDir and ImageDir bound what reconciliation may touch. See
	// internal/node/reclaim for why the boundary is a directory rather than a
	// list of resources.
	BaseDir  string
	ImageDir string

	mgr *Manager

	nodeToken string
	interval  time.Duration

	// reclaimed records that host reconciliation has run. It is a startup task,
	// not a periodic one: a reconnect to the control plane is not evidence that
	// anything on the host was orphaned, and re-running on every session would
	// mean racing the sandboxes this process has since created.
	reclaimed bool
}

func NewRegistrar(mgr *Manager, controlPlane, nodeID, region, bootstrapToken string,
	labels map[string]string, runtimes []string, res *nodev1.NodeResources) *Registrar {
	return &Registrar{
		ControlPlane:   controlPlane,
		NodeID:         nodeID,
		Region:         region,
		Labels:         labels,
		BootstrapToken: bootstrapToken,
		Resources:      res,
		Runtimes:       runtimes,
		mgr:            mgr,
		interval:       3 * time.Second,
	}
}

// Run registers and then heartbeats until ctx is cancelled, reconnecting
// with backoff. It returns only when ctx is done.
func (r *Registrar) Run(ctx context.Context) error {
	conn, err := grpc.NewClient(r.ControlPlane, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer conn.Close()
	client := nodev1.NewNodeServiceClient(conn)

	backoff := time.Second
	for ctx.Err() == nil {
		if err := r.session(ctx, client); err != nil && ctx.Err() == nil {
			slog.Warn("control-plane session ended", logging.KeyError, err, "retryIn", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
	return ctx.Err()
}

// session performs one register -> reconcile -> heartbeat cycle.
func (r *Registrar) session(ctx context.Context, client nodev1.NodeServiceClient) error {
	labels := r.Labels
	if r.Advertise != "" {
		// Carry the data-plane address in labels so the control plane can
		// route to this node without a separate discovery mechanism.
		labels = make(map[string]string, len(r.Labels)+1)
		for k, v := range r.Labels {
			labels[k] = v
		}
		labels[LabelAdvertiseAddr] = r.Advertise
	}
	resp, err := client.Register(ctx, &nodev1.RegisterRequest{
		BootstrapToken: r.BootstrapToken,
		NodeId:         r.NodeID,
		Region:         r.Region,
		Labels:         labels,
		Capabilities:   &nodev1.NodeCapabilities{Runtimes: r.Runtimes},
		Resources:      r.Resources,
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	r.nodeToken = resp.NodeToken
	if resp.HeartbeatIntervalSeconds > 0 {
		r.interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
	}
	slog.Info("registered with control plane", logging.KeyNode, r.NodeID, "region", r.Region)

	if err := r.reconcile(ctx, client); err != nil {
		slog.Error("reconcile failed", logging.KeyError, err)
	}

	stream, err := client.Heartbeat(ctx)
	if err != nil {
		return fmt.Errorf("open heartbeat: %w", err)
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := stream.Send(&nodev1.HeartbeatRequest{
				NodeId:    r.NodeID,
				NodeToken: r.nodeToken,
				Sandboxes: r.mgr.Statuses(),
				Usage:     r.usage(),
				// The node is the authority on what it holds, so it reports its
				// image cache rather than the control plane inferring it. Image
				// affinity and prewarm progress both read this.
				CachedImages: r.mgr.CachedImages(),
			}); err != nil {
				return fmt.Errorf("heartbeat send: %w", err)
			}
			if _, err := stream.Recv(); err != nil {
				return fmt.Errorf("heartbeat recv: %w", err)
			}
		}
	}
}

// reconcile compares control-plane expectations against local state and
// destroys orphans. Missing sandboxes are reported by omission in the next
// heartbeat; the control plane decides whether to recreate them.
func (r *Registrar) reconcile(ctx context.Context, client nodev1.NodeServiceClient) error {
	resp, err := client.SyncState(ctx, &nodev1.SyncStateRequest{
		NodeId: r.NodeID, NodeToken: r.nodeToken,
	})
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, spec := range resp.Expected {
		expected[spec.SandboxId] = true
	}
	for _, st := range r.mgr.Statuses() {
		if !expected[st.SandboxId] {
			slog.Info("destroying orphan sandbox", logging.KeySandbox, st.SandboxId)
			if err := r.mgr.Destroy(ctx, st.SandboxId, true); err != nil {
				slog.Error("cannot destroy orphan sandbox", logging.KeySandbox, st.SandboxId, logging.KeyError, err)
			}
		}
	}

	// Host resources come after the sandbox pass, and only once. The destroys
	// above release what this process knows about, so anything still on the host
	// afterwards has no owner in memory — which is exactly the population
	// reconciliation is looking for. Running it before the destroys would find
	// those resources still mapped and correctly decline to touch them.
	r.reclaimHost(expected)
	return nil
}

// reclaimHost returns host resources left by a previous noded, once, using the
// control plane's expected set to tell an orphan from a sandbox that predates
// this process.
//
// The expected set is what makes this safe, so a failure to obtain it must not
// reach here: reconcile only calls this after SyncState has succeeded. A
// reconciliation pass run against an empty set because a lookup failed would
// classify every running sandbox on the node as garbage.
func (r *Registrar) reclaimHost(expected map[string]bool) {
	if r.ReclaimHost == nil || r.reclaimed {
		return
	}
	r.reclaimed = true
	rec := &reclaim.Reconciler{
		BaseDir:  r.BaseDir,
		ImageDir: r.ImageDir,
		Host:     r.ReclaimHost,
		Metrics:  r.mgr.Metrics(),
	}
	if _, err := rec.Run(expected); err != nil {
		// Not fatal. A node that cannot reconcile still runs sandboxes; it just
		// keeps holding whatever the previous process leaked, which is the
		// behaviour before this existed.
		slog.Error("host resource reconciliation failed", logging.KeyError, err)
	}
}

func (r *Registrar) usage() *nodev1.NodeUsage {
	var cpu float64
	var mem int64
	for _, st := range r.mgr.Statuses() {
		if st.State != string(runtime.StateRunning) && st.State != string(runtime.StatePaused) {
			continue
		}
		if spec := r.mgr.SpecOf(st.SandboxId); spec != nil {
			cpu += spec.Cpu
			mem += spec.MemoryMib
		}
	}
	// Disk is measured rather than summed. CPU and memory commitments are close
	// enough to real usage to be worth summing, but a sandbox's disk request is
	// nominal: the sparse layer behind a 20 GiB request holds kilobytes, so adding
	// the requests up would overstate the node by orders of magnitude.
	return &nodev1.NodeUsage{
		CpuCommitted:       cpu,
		MemoryCommittedMib: mem,
		DiskUsedMib:        r.mgr.DiskUsedMiB(),
	}
}
