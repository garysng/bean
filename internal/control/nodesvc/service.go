// Package nodesvc implements the control-plane side of NodeService:
// node registration, heartbeats and state reconciliation. Nodes dial in
// (outbound), so this is the only inbound surface they need.
package nodesvc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
)

// SandboxLister supplies the expected sandbox set for a node (SyncState).
type SandboxLister interface {
	ExpectedForNode(nodeID string) []*nodev1.SandboxSpec
}

// LostHandler is notified when a node's lease expires so the control plane
// can mark its sandboxes LOST.
type LostHandler func(nodeID string)

// Service implements nodev1.NodeServiceServer.
//
// Node state is persisted rather than held here, so any gateway replica can
// serve a node's heartbeat and any replica can route to it. The only
// in-memory state is the node-token map, which is a cache: a node whose
// token this replica has not seen re-registers, which is cheap.
type Service struct {
	nodev1.UnimplementedNodeServiceServer

	store          *store.Store
	sched          *scheduler.Scheduler
	bootstrapToken string
	lister         SandboxLister
	onLost         LostHandler

	heartbeatInterval time.Duration

	mu     sync.Mutex
	tokens map[string]string // nodeID -> issued node token
}

type Options struct {
	BootstrapToken    string
	HeartbeatInterval time.Duration
	Lister            SandboxLister
	OnLost            LostHandler
}

func New(st *store.Store, sched *scheduler.Scheduler, opts Options) *Service {
	hb := opts.HeartbeatInterval
	if hb <= 0 {
		hb = 3 * time.Second
	}
	return &Service{
		store:             st,
		sched:             sched,
		bootstrapToken:    opts.BootstrapToken,
		lister:            opts.Lister,
		onLost:            opts.OnLost,
		heartbeatInterval: hb,
		tokens:            map[string]string{},
	}
}

// Register validates the bootstrap token, records the node with the
// scheduler and issues a node token used by subsequent calls.
func (s *Service) Register(ctx context.Context, req *nodev1.RegisterRequest) (*nodev1.RegisterResponse, error) {
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id required")
	}
	if req.GetRegion() == "" {
		return nil, status.Error(codes.InvalidArgument, "region required")
	}
	if s.bootstrapToken != "" &&
		subtle.ConstantTimeCompare([]byte(req.GetBootstrapToken()), []byte(s.bootstrapToken)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "invalid bootstrap token")
	}

	caps := req.GetCapabilities()
	res := req.GetResources()
	if res == nil || res.CpuAllocatable <= 0 || res.MemoryAllocatableMib <= 0 {
		return nil, status.Error(codes.InvalidArgument, "resources required")
	}

	// The advertise address travels in labels so registration alone tells
	// the control plane how to reach the node's data plane.
	advertise := req.Labels[node.LabelAdvertiseAddr]
	if err := s.store.UpsertNode(&store.NodeRecord{
		ID:                req.NodeId,
		Region:            req.Region,
		Labels:            req.Labels,
		Runtimes:          caps.GetRuntimes(),
		CPUAllocatable:    res.CpuAllocatable,
		MemoryAllocateMiB: res.MemoryAllocatableMib,
		DiskAllocateMiB:   res.DiskSandboxesMib,
		GPUCount:          res.GpuCount,
		State:             scheduler.NodeReady,
		AdvertiseAddr:     advertise,
		LastHeartbeat:     time.Now(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "persist node: %v", err)
	}

	token := "nt_" + randHex(16)
	s.mu.Lock()
	s.tokens[req.NodeId] = token
	s.mu.Unlock()

	log.Printf("node %s registered (region=%s runtimes=%v)", req.NodeId, req.Region, caps.GetRuntimes())
	return &nodev1.RegisterResponse{
		NodeToken:                token,
		HeartbeatIntervalSeconds: int64(s.heartbeatInterval.Seconds()),
	}, nil
}

// Heartbeat is a bidirectional stream: nodes push status, the control
// plane confirms the lease.
func (s *Service) Heartbeat(stream nodev1.NodeService_HeartbeatServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.authNode(req.GetNodeId(), req.GetNodeToken()); err != nil {
			return err
		}
		if err := s.store.TouchNode(req.NodeId, nil); err != nil {
			return status.Errorf(codes.Internal, "touch node: %v", err)
		}
		if err := stream.Send(&nodev1.HeartbeatResponse{LeaseOk: true}); err != nil {
			return err
		}
	}
}

// SyncState returns the expected sandbox set so a restarted node can
// reconcile local state against the control plane.
func (s *Service) SyncState(ctx context.Context, req *nodev1.SyncStateRequest) (*nodev1.SyncStateResponse, error) {
	if err := s.authNode(req.GetNodeId(), req.GetNodeToken()); err != nil {
		return nil, err
	}
	var expected []*nodev1.SandboxSpec
	if s.lister != nil {
		expected = s.lister.ExpectedForNode(req.NodeId)
	}
	return &nodev1.SyncStateResponse{Expected: expected}, nil
}

func (s *Service) authNode(nodeID, token string) error {
	if nodeID == "" {
		return status.Error(codes.InvalidArgument, "node_id required")
	}
	s.mu.Lock()
	want, ok := s.tokens[nodeID]
	s.mu.Unlock()
	if !ok {
		return status.Errorf(codes.Unauthenticated, "node %s not registered", nodeID)
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid node token")
	}
	return nil
}

// NodeAddr returns a node's data-plane address from the store, so any
// replica can route to any node without having handled its registration.
func (s *Service) NodeAddr(nodeID string) (string, bool) {
	n, err := s.store.GetNode(nodeID)
	if err != nil || n == nil || n.AdvertiseAddr == "" {
		return "", false
	}
	return n.AdvertiseAddr, true
}

// RunLivenessSweep drives lease expiry and reclaims leaked reservations
// until ctx is cancelled. Several replicas may sweep concurrently: the
// store reports whether a state transition actually happened, so each lost
// node is handled exactly once.
func (s *Service) RunLivenessSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			lost, err := s.sched.SweepLiveness()
			if err != nil {
				log.Printf("liveness sweep: %v", err)
				continue
			}
			for _, nodeID := range lost {
				log.Printf("node %s lease expired -> LOST", nodeID)
				if s.onLost != nil {
					s.onLost(nodeID)
				}
			}
			// A gateway that died mid-create would otherwise leak that
			// node's capacity permanently.
			if n, err := s.sched.ReclaimOrphanReservations(); err != nil {
				log.Printf("reclaim orphan reservations: %v", err)
			} else if n > 0 {
				log.Printf("reclaimed %d orphan reservation(s)", n)
			}
		}
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
