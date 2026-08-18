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
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
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
// Store is what node registration needs: the node registry, and nothing else. A
// registration path that could write a sandbox record or move a reservation would be
// able to contradict the scheduler, and the narrowing is what makes that impossible
// rather than merely unusual.
type Store interface {
	store.Nodes
}

type Service struct {
	nodev1.UnimplementedNodeServiceServer

	store          Store
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

func New(st Store, sched *scheduler.Scheduler, opts Options) *Service {
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
		CPUVendor:         res.CpuVendor,
		CPUFamily:         res.CpuFamily,
		CPUTemplate:       res.CpuTemplate,
		// Zero from a node that predates the field, which UpsertNode fills with the
		// old fixed default so an old node keeps its old behaviour.
		MaxCreates:    int(res.GetMaxCreates()),
		State:         scheduler.NodeReady,
		AdvertiseAddr: advertise,
		LastHeartbeat: time.Now(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "persist node: %v", err)
	}

	token := "nt_" + randHex(16)
	s.mu.Lock()
	s.tokens[req.NodeId] = token
	s.mu.Unlock()

	slog.Info("node registered", logging.KeyNode, req.NodeId, "region", req.Region, "runtimes", caps.GetRuntimes())
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
		// A heartbeat is liveness and nothing else: renew the lease against the
		// identity we just authenticated. What the node holds -- its disk usage and
		// image inventory -- arrives through UpdateNodeStatus, on its own schedule
		// and off this path, so a slow status report can never stall a renewal and
		// cost the node its lease.
		if err := s.store.RenewLease(req.GetNodeId()); err != nil {
			return status.Errorf(codes.Internal, "renew lease: %v", err)
		}
		if err := stream.Send(&nodev1.HeartbeatResponse{LeaseOk: true}); err != nil {
			return err
		}
	}
}

// UpdateNodeStatus records what a node holds. See UpdateNodeStatus in node.proto
// for why this is not part of the heartbeat.
//
// An absent category leaves that category alone. The node sends only what it has
// something to say about, so treating a missing field as an empty one would clear
// the control plane's view of a node's images the first time an unrelated
// category was reported on its own -- and the symptom would be image affinity
// that works only sometimes, which is the hardest kind of scheduling bug to
// attribute.
func (s *Service) UpdateNodeStatus(ctx context.Context, req *nodev1.UpdateNodeStatusRequest) (
	*nodev1.UpdateNodeStatusResponse, error) {

	if err := s.authNode(req.GetNodeId(), req.GetNodeToken()); err != nil {
		return nil, err
	}
	if inv := req.GetImages(); inv != nil {
		images := make(map[string]store.CachedImage, len(inv.GetImages()))
		for ref, img := range inv.GetImages() {
			images[ref] = store.CachedImage{
				SizeBytes: img.GetSizeBytes(),
				Digest:    img.GetDigest(),
				Warm:      img.GetWarm(),
			}
		}
		if err := s.store.PutNodeImages(req.GetNodeId(), images); err != nil {
			return nil, status.Errorf(codes.Internal, "record node images: %v", err)
		}
	}
	// Disk usage rides here now rather than on the heartbeat. Present on every
	// report (the node always measures it), unlike the image inventory which is
	// sent only when it has something to say -- so unlike images, an absent usage
	// message is a node that predates this change, and skipping the write leaves
	// its last figure rather than clobbering it with a zero.
	if u := req.GetUsage(); u != nil {
		if err := s.store.SetNodeUsage(req.GetNodeId(), u.GetDiskUsedMib(),
			u.GetCpuUsedPercent(), u.GetMemUsedPercent()); err != nil {
			return nil, status.Errorf(codes.Internal, "record node usage: %v", err)
		}
	}
	return &nodev1.UpdateNodeStatusResponse{}, nil
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
				slog.Error("liveness sweep failed", logging.KeyError, err)
				continue
			}
			for _, nodeID := range lost {
				slog.Warn("node lease expired", logging.KeyNode, nodeID, "state", "LOST")
				if s.onLost != nil {
					s.onLost(nodeID)
				}
			}
			// A gateway that died mid-create would otherwise leak that
			// node's capacity permanently.
			if n, err := s.sched.ReclaimOrphanReservations(); err != nil {
				slog.Error("cannot reclaim orphan reservations", logging.KeyError, err)
			} else if n > 0 {
				slog.Info("reclaimed orphan reservations", "count", n)
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
