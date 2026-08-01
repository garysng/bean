package node

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

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

	mgr *Manager

	nodeToken string
	interval  time.Duration
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
			log.Printf("control-plane session ended: %v (retrying in %s)", err, backoff)
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
	resp, err := client.Register(ctx, &nodev1.RegisterRequest{
		BootstrapToken: r.BootstrapToken,
		NodeId:         r.NodeID,
		Region:         r.Region,
		Labels:         r.Labels,
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
	log.Printf("registered with control plane as %s (region=%s)", r.NodeID, r.Region)

	if err := r.reconcile(ctx, client); err != nil {
		log.Printf("reconcile: %v", err)
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
			log.Printf("reconcile: destroying orphan sandbox %s", st.SandboxId)
			if err := r.mgr.Destroy(ctx, st.SandboxId, true); err != nil {
				log.Printf("reconcile: destroy %s: %v", st.SandboxId, err)
			}
		}
	}
	return nil
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
	return &nodev1.NodeUsage{CpuCommitted: cpu, MemoryCommittedMib: mem}
}
