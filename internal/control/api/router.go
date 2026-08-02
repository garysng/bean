package api

import (
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/obs"
)

// NodeResolver maps a node id to its data-plane address.
type NodeResolver interface {
	NodeAddr(nodeID string) (string, bool)
}

// NodeRouter hands out SandboxService clients per node, reusing one gRPC
// connection each. Connections are created lazily and kept for the process
// lifetime; gRPC handles reconnection internally.
type NodeRouter struct {
	resolver  NodeResolver
	nodeToken string

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

func NewNodeRouter(resolver NodeResolver, nodeToken string) *NodeRouter {
	return &NodeRouter{resolver: resolver, nodeToken: nodeToken, conns: map[string]*grpc.ClientConn{}}
}

// Client returns a SandboxService client for the given node.
func (r *NodeRouter) Client(nodeID string) (nodev1.SandboxServiceClient, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("empty node id")
	}
	r.mu.Lock()
	conn, ok := r.conns[nodeID]
	r.mu.Unlock()
	if ok {
		return nodev1.NewSandboxServiceClient(conn), nil
	}

	addr, ok := r.resolver.NodeAddr(nodeID)
	if !ok {
		return nil, fmt.Errorf("node %s has no known address", nodeID)
	}
	unary, stream := node.TokenClientInterceptors(r.nodeToken)
	// Trace injection runs after the token interceptor so both sets of
	// metadata reach the node; chaining is required because gRPC keeps only
	// the last WithUnaryInterceptor.
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(unary, obs.UnaryClientTrace()),
		grpc.WithChainStreamInterceptor(stream, obs.StreamClientTrace()))
	if err != nil {
		return nil, fmt.Errorf("dial node %s at %s: %w", nodeID, addr, err)
	}

	r.mu.Lock()
	// Another goroutine may have won the race; keep a single connection.
	if existing, ok := r.conns[nodeID]; ok {
		r.mu.Unlock()
		conn.Close()
		return nodev1.NewSandboxServiceClient(existing), nil
	}
	r.conns[nodeID] = conn
	r.mu.Unlock()
	return nodev1.NewSandboxServiceClient(conn), nil
}

// Close tears down all pooled connections.
func (r *NodeRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.conns {
		_ = c.Close()
		delete(r.conns, id)
	}
}

// Evict drops a node's connection (node lost or drained).
func (r *NodeRouter) Evict(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.conns[nodeID]; ok {
		_ = c.Close()
		delete(r.conns, nodeID)
	}
}

// Router resolves the SandboxService client for a sandbox's node.
type Router interface {
	Client(nodeID string) (nodev1.SandboxServiceClient, error)
}
