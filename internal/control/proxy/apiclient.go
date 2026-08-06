package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APISandboxes resolves placement by asking bean-api, not by reading its database.
//
// The database is the wrong interface for two reasons, and only the second is fatal.
//
// The first is coupling: the store's Open runs migrations, so a second process opening
// it attempts DDL on a file another process owns -- measured as
// "database is locked (SQLITE_BUSY)", with the proxy failing to start and the log line
// reading like transient contention rather than a wrong call.
//
// The second is that a SQLite file does not cross machines. This proxy belongs near the
// nodes it forwards to, and bean-api belongs wherever the control plane runs; a path on
// disk cannot span them. Reading the database worked in a single-host development stack
// and would have had to be undone the first time anything was deployed for real.
//
// So placement is read through the API that already publishes it: the sandbox record
// carries nodeId, and the node record carries the forwarding address in its labels.
type APISandboxes struct {
	// BaseURL is bean-api, e.g. http://bean-api.internal:8080.
	BaseURL string
	// APIKey authenticates this proxy to bean-api. The proxy is a cluster component,
	// so it holds its own credential rather than forwarding a caller's.
	APIKey string

	HTTP *http.Client

	// Placement changes rarely relative to data-plane traffic -- a sandbox moves when
	// it is created or destroyed, not per request -- so it is cached briefly. Without
	// this, every request through the proxy is two extra round trips to the control
	// plane, which is most of what moving the data plane off it was meant to avoid.
	mu     sync.Mutex
	cache  map[string]cachedAddr
	nodes  map[string]cachedAddr
	forFor time.Duration
}

type cachedAddr struct {
	addr string
	at   time.Time
}

// NewAPISandboxes builds the resolver.
func NewAPISandboxes(baseURL, apiKey string, cacheFor time.Duration) *APISandboxes {
	return &APISandboxes{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		HTTP: &http.Client{
			// Bounded, unlike the proxied traffic itself: this is a control-plane
			// lookup, and a slow one should fail the request rather than hold a
			// connection open indefinitely.
			Timeout: 5 * time.Second,
		},
		cache:  map[string]cachedAddr{},
		nodes:  map[string]cachedAddr{},
		forFor: cacheFor,
	}
}

// NodeAddrFor returns the forwarding address of the node holding sandboxID.
func (a *APISandboxes) NodeAddrFor(sandboxID string) (string, error) {
	if addr, ok := a.cached(a.cache, sandboxID); ok {
		return addr, nil
	}

	nodeID, err := a.nodeOf(sandboxID)
	if err != nil {
		return "", err
	}
	addr, err := a.forwardAddrOf(nodeID)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	a.cache[sandboxID] = cachedAddr{addr: addr, at: time.Now()}
	a.mu.Unlock()
	return addr, nil
}

func (a *APISandboxes) cached(m map[string]cachedAddr, key string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := m[key]
	if !ok || time.Since(e.at) > a.forFor {
		return "", false
	}
	return e.addr, true
}

// nodeOf asks which node holds a sandbox.
func (a *APISandboxes) nodeOf(sandboxID string) (string, error) {
	var body struct {
		Sandbox struct {
			NodeID string `json:"nodeId"`
			State  string `json:"state"`
		} `json:"sandbox"`
	}
	status, err := a.get("/v1/sandboxes/"+sandboxID, &body)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrNoSandbox, sandboxID)
	default:
		return "", fmt.Errorf("bean-api returned %d for sandbox %s", status, sandboxID)
	}
	if body.Sandbox.NodeID == "" {
		// Known but unplaced: transient during a create, permanent for one whose
		// placement failed.
		return "", fmt.Errorf("%w: %s is not placed on a node", ErrNoSandbox, sandboxID)
	}
	return body.Sandbox.NodeID, nil
}

// forwardAddrOf reads a node's forwarding address out of its registration labels.
//
// The whole node list is fetched because bean-api has no per-node endpoint. Cached for
// the same window as placement, so this is not a per-request cost.
func (a *APISandboxes) forwardAddrOf(nodeID string) (string, error) {
	if addr, ok := a.cached(a.nodes, nodeID); ok {
		return addr, nil
	}

	var body struct {
		Nodes []struct {
			ID     string            `json:"id"`
			Labels map[string]string `json:"labels"`
		} `json:"nodes"`
	}
	status, err := a.get("/v1/nodes", &body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("bean-api returned %d for the node list", status)
	}

	now := time.Now()
	a.mu.Lock()
	for _, n := range body.Nodes {
		if addr := n.Labels[labelSandboxPortAddr]; addr != "" {
			a.nodes[n.ID] = cachedAddr{addr: addr, at: now}
		}
	}
	e, ok := a.nodes[nodeID]
	a.mu.Unlock()

	if !ok || e.addr == "" {
		return "", fmt.Errorf("%w: node %s advertises no forwarding address, so it "+
			"was started without --sandbox-port-listen", ErrNoForwarding, nodeID)
	}
	return e.addr, nil
}

// labelSandboxPortAddr duplicates node.LabelSandboxPortAddr rather than importing it.
//
// The alternative pulls the whole node package -- and with it the microVM runtime and
// its Linux-only files -- into a process that only speaks HTTP. The string is part of
// a published API response, so it is a wire format either way; a test asserts the two
// agree.
const labelSandboxPortAddr = "bean.io/sandbox-port-addr"

func (a *APISandboxes) get(path string, out any) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+path, nil)
	if err != nil {
		return 0, err
	}
	if a.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}

	resp, err := a.HTTP.Do(req)
	if err != nil {
		// A control plane that cannot be reached is not a missing sandbox, and the
		// distinction decides whether a caller should retry.
		return 0, fmt.Errorf("reach bean-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode bean-api response: %w", err)
	}
	return resp.StatusCode, nil
}

// Invalidate drops a sandbox's cached placement. Called when a forward fails, so a
// sandbox that moved is looked up again rather than retried against a stale node for
// the length of the cache window.
func (a *APISandboxes) Invalidate(sandboxID string) {
	a.mu.Lock()
	delete(a.cache, sandboxID)
	a.mu.Unlock()
}

var _ Sandboxes = (*APISandboxes)(nil)
