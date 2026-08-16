//go:build e2e

// Build-log e2e. Unlike the sandbox e2e, this stands up its own S3-wired stack
// (bean-api + a build-capable noded) so it can prove the properties the S3
// build-log refactor (docs/build-logs-s3.md, Step A) is about:
//
//  1. the node uploads a build's output to a dedicated S3 logs bucket, laid out
//     as buildlogs/<key>/NNNNNN chunks + a manifest;
//  2. a SECOND bean-api replica — one that never handled the build — serves
//     GET /logs by reading that bucket plus the shared store, so a logs request
//     that lands on any replica no longer 404s; and
//  3. POST /cancel on that other replica resolves the build's node from the
//     store record and calls the node's CancelBuild, stopping the build.
//
// It needs a real S3-compatible server and a buildkitd, so it SKIPS unless
// BEAN_S3_ENDPOINT is set (mirroring internal/control/s3's integration tests),
// keeping `make test-e2e` green on hosts without that infrastructure.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/s3"
)

func envDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// requireS3 skips a test when no S3 endpoint is configured. The build-log path
// has no DirStore-only shortcut worth exercising here: the point is the real
// node→S3→gateway round trip, which needs a server that accepts the SigV4 the
// hand-rolled client produces.
func requireS3(t *testing.T) (endpoint, region, bucket string) {
	t.Helper()
	endpoint = os.Getenv("BEAN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BEAN_S3_ENDPOINT not set; skipping build-log e2e")
	}
	region = envDefault("BEAN_S3_REGION", "us-east-1")
	bucket = envDefault("BEAN_S3_LOGS_BUCKET", "bean-build-logs-e2e")
	return
}

// buildCluster is a bean-api (replica "a", the one the node connects to) plus a
// build-capable noded, all sharing one store DB and one S3 logs bucket. More
// replicas that share the same DB+bucket are spawned with newReplica.
type buildCluster struct {
	t        *testing.T
	dir      string
	db       string
	endpoint string
	region   string
	bucket   string
	buildkit string
	baseImg  string

	// fc-runtime assets. Builds are only implemented by the fc tier's
	// ImageBuilder (internal/node/runtime/fc_linux.go), so a build-capable node
	// runs --runtime fc with these present -- not --runtime local, which has no
	// builder. Paths default to the .75 KVM host's layout (docs §14) and are
	// overridable for another host.
	fcBin     string
	kernel    string
	agentDisk string

	apiBin   string
	nodedBin string
	beandBin string

	nodeGRPC int // the port the node dials; replica "a" owns it
	replicas []*replica
	noded    *exec.Cmd
	s3c      *s3.Client
}

type replica struct {
	url      string
	cmd      *exec.Cmd
	httpPort int
	nodeGRPC int
}

func newBuildCluster(t *testing.T) *buildCluster {
	t.Helper()
	endpoint, region, bucket := requireS3(t)

	c := &buildCluster{
		t:        t,
		dir:      t.TempDir(),
		endpoint: endpoint,
		region:   region,
		bucket:   bucket,
		buildkit: envDefault("BEAN_BUILDKIT_ADDR", "unix:///run/bean/buildkitd.sock"),
		// Docker Hub is unreachable from the .75 KVM host; the daocloud mirror
		// serves library images. Override for a host with direct Hub access.
		baseImg:  envDefault("BEAN_E2E_BASE_IMAGE", "docker.m.daocloud.io/library/busybox"),
		fcBin:    envDefault("BEAN_FC_BIN", "/var/lib/bean/assets/firecracker"),
		kernel:   envDefault("BEAN_FC_KERNEL", "/var/lib/bean/assets/vmlinux-6.1.175"),
		agentDisk: envDefault("BEAN_FC_AGENT_DISK", "/var/lib/bean/assets/agent.ext4"),
	}
	c.db = filepath.Join(c.dir, "cluster.db")

	var err error
	if c.apiBin, err = build("bean-api"); err != nil {
		t.Fatal(err)
	}
	if c.nodedBin, err = build("noded"); err != nil {
		t.Fatal(err)
	}
	if c.beandBin, err = build("beand"); err != nil {
		t.Fatal(err)
	}

	// The logs bucket has to exist before the node writes to it: S3 PutObject to
	// a missing bucket is an error, and the node's writer swallows write errors
	// (a build should not fail because a log chunk did not upload), so a missing
	// bucket would surface only as silently empty logs.
	c.s3c, err = s3.New(s3.Config{
		Endpoint:  endpoint,
		Region:    region,
		AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	if err := c.s3c.EnsureBucket(context.Background(), bucket); err != nil {
		t.Fatalf("ensure logs bucket %s: %v", bucket, err)
	}

	c.nodeGRPC = mustFreePort(t)
	primary := c.startReplica() // replica "a": the node connects to this one

	nodeGRPCPort := mustFreePort(t)
	// The node runs the fc tier, not local: only the fc runtime's ImageBuilder
	// can build images (internal/node/manager.go's rt.(runtime.ImageBuilder)
	// assertion; local has no builder). Building never boots a microVM -- it
	// shells out to buildkit -- but NewFCTier still requires /dev/kvm plus the
	// firecracker/kernel/agent-disk assets to construct, so this test needs a
	// KVM host with the node assets built (docs/build-logs-s3.md §14).
	noded := exec.Command(c.nodedBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
		"--runtime", "fc",
		"--firecracker-bin", c.fcBin,
		"--kernel", c.kernel,
		"--agent-disk", c.agentDisk,
		"--agent-bin", c.beandBin,
		"--base-dir", filepath.Join(c.dir, "sandboxes"),
		"--image-dir", filepath.Join(c.dir, "images"),
		"--node-token", nodeToken,
		"--control-plane", fmt.Sprintf("127.0.0.1:%d", c.nodeGRPC),
		"--node-id", "build-node-0",
		"--region", "local",
		"--advertise", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
		"--cpu", "16", "--memory-mib", "16384",
		// the flags that make this a build-capable, S3-logging node:
		"--buildkit-addr", c.buildkit,
		"--s3-endpoint", c.endpoint,
		"--s3-region", c.region,
		"--s3-logs-bucket", c.bucket)
	// Credentials reach the child through the environment, never the command
	// line (they would otherwise leak via /proc/<pid>/cmdline), per s3-storage §6.
	noded.Env = os.Environ()
	noded.Stdout, noded.Stderr = os.Stderr, os.Stderr
	if err := noded.Start(); err != nil {
		t.Fatalf("start noded: %v", err)
	}
	c.noded = noded

	t.Cleanup(c.stop)
	waitNodeReady(t, primary.url, 30*time.Second)
	return c
}

// startReplica launches a bean-api sharing this cluster's DB and logs bucket.
// The first one owns nodeGRPC (the node connects to it); later ones get an
// unused node-grpc port, so they carry no node yet still serve /logs and
// /cancel from the shared store + bucket — exactly the multi-replica case.
func (c *buildCluster) startReplica() *replica {
	c.t.Helper()
	httpPort := mustFreePort(c.t)
	nodeGRPC := c.nodeGRPC
	if len(c.replicas) > 0 {
		nodeGRPC = mustFreePort(c.t)
	}
	r := c.spawnReplica(httpPort, nodeGRPC)
	c.replicas = append(c.replicas, r)
	return r
}

// spawnReplica launches one bean-api on the given ports and waits for it to be
// healthy. Split from startReplica so restart can relaunch on the SAME ports --
// which is what a real restart is, and what lets the node (which dials the
// control-plane port) reconnect to the replacement.
func (c *buildCluster) spawnReplica(httpPort, nodeGRPC int) *replica {
	c.t.Helper()
	cmd := exec.Command(c.apiBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--node-grpc", fmt.Sprintf("127.0.0.1:%d", nodeGRPC),
		"--region", "local",
		"--runtime-tier", "local",
		"--db", c.db,
		"--api-key", apiKey,
		"--node-token", nodeToken,
		"--s3-endpoint", c.endpoint,
		"--s3-region", c.region,
		"--s3-logs-bucket", c.bucket)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start bean-api: %v", err)
	}
	r := &replica{
		url:      fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		cmd:      cmd,
		httpPort: httpPort,
		nodeGRPC: nodeGRPC,
	}
	waitHealthy(c.t, r.url, 15*time.Second)
	return r
}

func (c *buildCluster) newReplica() *replica { return c.startReplica() }

// restart kills a replica and brings up a replacement on the same ports,
// simulating a bean-api process restart. The build it was polling keeps running
// on the node (node-owned context); the replacement's ReconcileBuilds must
// re-attach and drive the template to a terminal state.
func (c *buildCluster) restart(r *replica) *replica {
	c.t.Helper()
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_, _ = r.cmd.Process.Wait()
	}
	repl := c.spawnReplica(r.httpPort, r.nodeGRPC)
	for i, existing := range c.replicas {
		if existing == r {
			c.replicas[i] = repl
			break
		}
	}
	return repl
}

func (c *buildCluster) stop() {
	procs := []*exec.Cmd{c.noded}
	for _, r := range c.replicas {
		procs = append(procs, r.cmd)
	}
	for _, p := range procs {
		if p != nil && p.Process != nil {
			_ = p.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, p := range procs {
		if p == nil {
			continue
		}
		done := make(chan struct{})
		go func(cmd *exec.Cmd) { cmd.Wait(); close(done) }(p)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if p.Process != nil {
				_ = p.Process.Kill()
			}
		}
	}
}

// dockerfile builds a Dockerfile that emits a unique marker and runs for about
// `secs` seconds, long enough to span several of the writer's 2s time-flushes.
func (c *buildCluster) dockerfile(marker string, secs int) string {
	return fmt.Sprintf("FROM %s\nRUN echo %s && sleep %d && echo %s-DONE\n",
		c.baseImg, marker, secs, marker)
}

func mustFreePort(t *testing.T) int {
	t.Helper()
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitNodeReady(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		code, body := apiGet(t, url, "/v1/nodes")
		if code == 200 {
			var b struct {
				Nodes []struct {
					State string `json:"state"`
				} `json:"nodes"`
			}
			json.Unmarshal(body, &b)
			for _, n := range b.Nodes {
				if n.State == "READY" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready node on %s within %s", url, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- HTTP helpers (per-URL, so a specific replica can be targeted) ---

func apiGet(t *testing.T, url, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+path, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func apiPostJSON(t *testing.T, url, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", url+path, rd)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func refQuery(tag string) string {
	return "ref=" + strings.ReplaceAll(strings.ReplaceAll(tag, "/", "%2F"), ":", "%3A")
}

// followLogs reads GET /logs to completion (follow defaults true, so it blocks
// until the build reaches a terminal state and the outcome trailer is written).
func followLogs(t *testing.T, url, tag string, timeout time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest("GET", url+"/v1/templates/build/logs?"+refQuery(tag), nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("follow logs: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// snapshotLogs reads current logs without following, returning (status, body).
func snapshotLogs(t *testing.T, url, tag string) (int, string) {
	t.Helper()
	code, b := apiGet(t, url, "/v1/templates/build/logs?"+refQuery(tag)+"&follow=false")
	return code, string(b)
}

func templateState(t *testing.T, url, tag string) (string, string) {
	t.Helper()
	code, b := apiGet(t, url, "/v1/templates/status?name="+strings.ReplaceAll(strings.ReplaceAll(tag, "/", "%2F"), ":", "%3A"))
	if code != 200 {
		return "", ""
	}
	var m struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	json.Unmarshal(b, &m)
	return m.State, m.Reason
}

func waitTerminal(t *testing.T, url, tag string, timeout time.Duration) (string, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, reason := templateState(t, url, tag)
		if st == "READY" || st == "FAILED" {
			return st, reason
		}
		if time.Now().After(deadline) {
			t.Fatalf("template %s not terminal within %s (state=%q)", tag, timeout, st)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func startBuild(t *testing.T, url, tag, dockerfile string) {
	t.Helper()
	code, b := apiPostJSON(t, url, "/v1/templates/build", map[string]any{
		"tag": tag, "dockerfile": dockerfile,
	})
	if code != 202 {
		t.Fatalf("start build %s: %d %s", tag, code, b)
	}
}

func uniqueTag(base string) string {
	return fmt.Sprintf("e2e/%s-%d:v1", base, time.Now().UnixNano())
}

// --- tests ---

// TestBuildLogsLandInS3 proves the write path: a build's output is uploaded to
// the dedicated bucket as buildlogs/<key>/NNNNNN chunks with a terminal
// manifest, and the gateway reassembles it (markers appear in order) and the
// template reaches READY.
func TestBuildLogsLandInS3(t *testing.T) {
	c := newBuildCluster(t)
	primary := c.replicas[0]
	tag := uniqueTag("blog-land")

	startBuild(t, primary.url, tag, c.dockerfile("BEANMARK", 5))
	body := followLogs(t, primary.url, tag, 180*time.Second)

	if !strings.Contains(body, "BEANMARK") {
		t.Fatalf("log body missing marker; got:\n%s", body)
	}

	st, reason := waitTerminal(t, primary.url, tag, 180*time.Second)
	if st != "READY" {
		t.Fatalf("build did not reach READY: state=%s reason=%q\nlogs:\n%s", st, reason, body)
	}

	// The bucket must actually hold the chunk + manifest layout.
	key, err := s3.BuildLogKey(tag)
	if err != nil {
		t.Fatalf("build log key: %v", err)
	}
	ctx := context.Background()
	if _, err := c.s3c.HeadObject(ctx, c.bucket, fmt.Sprintf("buildlogs/%s/%06d", key, 0)); err != nil {
		t.Fatalf("first log chunk buildlogs/%s/000000 missing: %v", key, err)
	}
	rc, err := c.s3c.GetObject(ctx, c.bucket, "buildlogs/"+key+"/manifest")
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	mb, _ := io.ReadAll(rc)
	rc.Close()
	var man s3.BuildLogManifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatalf("manifest parse: %v (%s)", err, mb)
	}
	if !man.Done {
		t.Errorf("manifest not marked done: %+v", man)
	}
	if man.Chunks < 1 {
		t.Errorf("manifest reports %d chunks, want >=1", man.Chunks)
	}
}

// TestBuildLogsServedFromOtherReplica proves the multi-replica read fix: a
// second bean-api — which never handled the build and has no node attached —
// serves the build's logs by reading the shared store + bucket, where the old
// in-memory buildTracker would have returned BUILD_NOT_FOUND.
func TestBuildLogsServedFromOtherReplica(t *testing.T) {
	c := newBuildCluster(t)
	primary := c.replicas[0]
	other := c.newReplica()
	tag := uniqueTag("blog-replica")

	startBuild(t, primary.url, tag, c.dockerfile("REPLICAMARK", 12))

	// Poll the OTHER replica until it serves the marker. Anything but a 5xx/404
	// from a replica that never saw the build is the property under test.
	deadline := time.Now().Add(60 * time.Second)
	var lastCode int
	var lastBody string
	for time.Now().Before(deadline) {
		code, body := snapshotLogs(t, other.url, tag)
		lastCode, lastBody = code, body
		if code == 200 && strings.Contains(body, "REPLICAMARK") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastCode != 200 || !strings.Contains(lastBody, "REPLICAMARK") {
		t.Fatalf("other replica did not serve logs: code=%d body=%q", lastCode, lastBody)
	}

	// And the build still completes (the primary held the result stream the
	// whole time; the other replica only read logs).
	if st, reason := waitTerminal(t, primary.url, tag, 180*time.Second); st != "READY" {
		t.Fatalf("build not READY: state=%s reason=%q", st, reason)
	}
}

// TestBuildCancelFromOtherReplica proves node-owned cancel across replicas: a
// cancel issued to a replica that did not start the build resolves the build's
// node from the store record and calls the node's CancelBuild, stopping it.
func TestBuildCancelFromOtherReplica(t *testing.T) {
	c := newBuildCluster(t)
	primary := c.replicas[0]
	other := c.newReplica()
	tag := uniqueTag("blog-cancel")

	// A long build so there is a wide window to cancel it mid-run.
	startBuild(t, primary.url, tag, c.dockerfile("CANCELMARK", 120))

	// Wait until the build is actually running (logs show the marker) before
	// cancelling, so we are cancelling a live build, not a queued one.
	deadline := time.Now().Add(60 * time.Second)
	running := false
	for time.Now().Before(deadline) {
		if code, body := snapshotLogs(t, other.url, tag); code == 200 && strings.Contains(body, "CANCELMARK") {
			running = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !running {
		t.Fatal("build never reached running state to cancel")
	}

	code, b := apiPostJSON(t, other.url, "/v1/templates/build/cancel?"+refQuery(tag), nil)
	if code != 202 {
		t.Fatalf("cancel from other replica: %d %s", code, b)
	}

	// The build must stop well before its 120s sleep would end.
	st, reason := waitTerminal(t, primary.url, tag, 60*time.Second)
	if st != "FAILED" {
		t.Fatalf("cancelled build state=%s reason=%q, want FAILED", st, reason)
	}
	if !strings.Contains(strings.ToLower(reason), "cancel") {
		t.Errorf("cancel reason = %q, want it to mention cancellation", reason)
	}
}

// TestBuildSurvivesReplicaRestart proves the Step B property: a build runs under
// the node's own context, so killing the bean-api that started it does not stop
// the build, and the restarted replica's ReconcileBuilds re-attaches by polling
// the node and drives the template to READY. Under the old stream model the
// build's lifeline was the originating call's context, so a restart mid-build
// stranded the template in BUILDING forever (docs/build-logs-s3.md §8, §14).
func TestBuildSurvivesReplicaRestart(t *testing.T) {
	c := newBuildCluster(t)
	primary := c.replicas[0]
	tag := uniqueTag("blog-restart")

	// A build long enough that we can reliably kill the gateway while it runs.
	startBuild(t, primary.url, tag, c.dockerfile("RESTARTMARK", 25))

	// Wait until the build is actually running on the node (marker present),
	// then kill and replace the gateway that started it.
	deadline := time.Now().Add(60 * time.Second)
	running := false
	for time.Now().Before(deadline) {
		if code, body := snapshotLogs(t, primary.url, tag); code == 200 && strings.Contains(body, "RESTARTMARK") {
			running = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !running {
		t.Fatal("build never reached running state before restart")
	}

	// Kill the gateway mid-build and bring up a replacement on the same ports.
	restarted := c.restart(primary)

	// The restarted replica must re-attach (ReconcileBuilds) and record the
	// terminal state. The build kept running on the node the whole time.
	st, reason := waitTerminal(t, restarted.url, tag, 180*time.Second)
	if st != "READY" {
		t.Fatalf("build did not reach READY after restart: state=%s reason=%q", st, reason)
	}

	// And the logs survived the restart -- served statelessly from S3 by the
	// replacement, which never handled the original build request.
	code, body := snapshotLogs(t, restarted.url, tag)
	if code != 200 || !strings.Contains(body, "RESTARTMARK") {
		t.Fatalf("restarted replica did not serve logs: code=%d body=%q", code, body)
	}
}
