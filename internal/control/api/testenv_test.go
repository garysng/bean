package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/s3"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/runtime"
)

const testKey = "bk_test_secret"

// agentBin is the in-sandbox daemon the local runtime launches. Tests build
// it once so every sandbox they create is a real process running the real
// agent, not a stub.
var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bean-beand-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "beand")
	if out, err := exec.Command("go", "build", "-o", agentBin,
		"github.com/garysng/bean/cmd/beand").CombinedOutput(); err != nil {
		panic("build beand: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testEnv is a complete control plane over real nodes: store, scheduler,
// node routing and the gateway. There is one shape of stack because there is
// one code path — a single-node cluster is just a cluster with one node.
type testEnv struct {
	T       *testing.T
	Server  *httptest.Server
	Store     *store.Store
	Sched     *scheduler.Scheduler
	Blobs     snapshot.Blobs
	Images    *image.Service
	BuildLogs s3.ObjectStore
	API       *Server
	NodeIDs []string

	resolver *mapResolver
}

// envOpts tunes the test environment.
type envOpts struct {
	// Nodes is how many nodes to start and register. Zero means one node.
	Nodes int
	// CPUPerNode and MemoryPerNode bound capacity, for exercising placement
	// pressure. Zero means generous defaults.
	CPUPerNode    float64
	MemoryPerNode int64
	// WithSecrets enables credential encryption.
	WithSecrets bool
	// WithoutSnapshots disables snapshot storage so the endpoints report
	// unavailable rather than pretending.
	WithoutSnapshots bool
	// WithoutImages disables the image service.
	WithoutImages bool
	// ImagePolicy restricts which images a create may use. The zero value
	// permits everything, matching an unconfigured deployment.
	ImagePolicy image.Policy
	// WithIdentity attributes images to the caller named by the owner header,
	// standing in for the external layer that will authenticate for real.
	WithIdentity bool
}

// mapResolver resolves node ids to data-plane addresses.
type mapResolver struct {
	mu    sync.Mutex
	addrs map[string]string
}

func (m *mapResolver) NodeAddr(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.addrs[id]
	return a, ok
}

func (m *mapResolver) set(id, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addrs[id] = addr
}

// startEnv brings up the stack described by opts.
func startEnv(t *testing.T, opts envOpts) *testEnv {
	t.Helper()
	if opts.Nodes == 0 {
		opts.Nodes = 1
	}
	if opts.CPUPerNode == 0 {
		opts.CPUPerNode = 64
	}
	if opts.MemoryPerNode == 0 {
		opts.MemoryPerNode = 1 << 20
	}

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "bean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	env := &testEnv{
		T: t, Store: st,
		Sched:    scheduler.New(st, scheduler.DefaultWeights()),
		resolver: &mapResolver{addrs: map[string]string{}},
	}

	for i := 0; i < opts.Nodes; i++ {
		id := "node-" + string(rune('a'+i))
		addr := startTestNode(t)
		env.resolver.set(id, addr)
		if err := st.UpsertNode(&store.NodeRecord{
			ID: id, Region: "local", Runtimes: []string{"local"},
			CPUAllocatable: opts.CPUPerNode, MemoryAllocateMiB: opts.MemoryPerNode,
			DiskAllocateMiB: 1 << 20, MaxCreates: 64,
			CachedImages: map[string]store.CachedImage{},
			State:        scheduler.NodeReady, AdvertiseAddr: addr,
			LastHeartbeat: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		env.NodeIDs = append(env.NodeIDs, id)
	}

	apiOpts := Options{Region: "local", APIKey: testKey, RuntimeTier: "local"}
	if !opts.WithoutImages {
		env.Images = image.NewWithPolicy(st, nil, opts.ImagePolicy)
		apiOpts.Images = env.Images
	}
	if opts.WithIdentity {
		apiOpts.Identity = OwnerFromHeader(OwnerHeader)
	}
	if !opts.WithoutSnapshots {
		blobs, err := snapshot.NewDirBlobs(filepath.Join(dir, "blobs"))
		if err != nil {
			t.Fatal(err)
		}
		env.Blobs = blobs
		apiOpts.Snapshots = blobs
	}
	if opts.WithSecrets {
		box, err := secret.NewBox("test-master-key")
		if err != nil {
			t.Fatal(err)
		}
		apiOpts.Secrets = box
	}

	logsStore, err := s3.NewDirStore(filepath.Join(dir, "build-logs"))
	if err != nil {
		t.Fatal(err)
	}
	env.BuildLogs = logsStore
	apiOpts.BuildLogs = logsStore

	router := NewNodeRouter(env.resolver, "")
	t.Cleanup(router.Close)
	env.API = New(st, router, env.Sched, apiOpts)
	env.Server = httptest.NewServer(env.API.Handler())
	t.Cleanup(env.Server.Close)
	return env
}

// startTestNode runs one noded over the local runtime and returns its
// address.
func startTestNode(t *testing.T) string {
	t.Helper()
	mgr := node.NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr, nil))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// nodeClient dials a node directly, for tests that need to bypass the
// gateway.
func (e *testEnv) nodeClient(nodeID string) nodev1.SandboxServiceClient {
	e.T.Helper()
	addr, ok := e.resolver.NodeAddr(nodeID)
	if !ok {
		e.T.Fatalf("unknown node %s", nodeID)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Cleanup(func() { conn.Close() })
	return nodev1.NewSandboxServiceClient(conn)
}

// do issues an authenticated JSON request and decodes the response.
func (e *testEnv) do(method, path string, body any) (*http.Response, map[string]any) {
	e.T.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.T.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.Server.URL+path, rd)
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// doAs issues an authenticated JSON request carrying an owner identity, the
// way the external platform layer is expected to.
func (e *testEnv) doAs(owner, method, path string, body any) (*http.Response, map[string]any) {
	e.T.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.T.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.Server.URL+path, rd)
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	if owner != "" {
		req.Header.Set(OwnerHeader, owner)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// raw issues an authenticated request and returns the body bytes, for
// endpoints that are not JSON (file reads, logs, metrics).
func (e *testEnv) raw(method, path, body string) (*http.Response, string) {
	e.T.Helper()
	req, err := http.NewRequest(method, e.Server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.String()
}

// createSandbox creates a sandbox and returns its record, failing the test
// if creation does not succeed.
func (e *testEnv) createSandbox(body map[string]any) map[string]any {
	e.T.Helper()
	if body == nil {
		body = map[string]any{"imageRef": "test:latest"}
	}
	resp, out := e.do("POST", "/v1/sandboxes", body)
	if resp.StatusCode != http.StatusCreated {
		e.T.Fatalf("create sandbox: %d %v", resp.StatusCode, out)
	}
	return out["sandbox"].(map[string]any)
}

// sandboxID creates a sandbox and returns just its id.
func (e *testEnv) sandboxID(body map[string]any) string {
	e.T.Helper()
	return e.createSandbox(body)["id"].(string)
}

// state reads a sandbox's current state through the API.
func (e *testEnv) state(id string) string {
	e.T.Helper()
	_, out := e.do("GET", "/v1/sandboxes/"+id, nil)
	sb, ok := out["sandbox"].(map[string]any)
	if !ok {
		return ""
	}
	return sb["state"].(string)
}
