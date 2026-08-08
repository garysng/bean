//go:build e2e

// Package e2e runs the full stack as real processes: noded (local
// runtime) + bean-api, exercised via REST and the bean CLI binary.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	apiKey    = "bk_e2e_key"
	nodeToken = "nt_e2e_token"
)

var (
	apiURL  string
	cliBin  string
	binDir  string
	daemons []*exec.Cmd
)

func TestMain(m *testing.M) {
	var err error
	binDir, err = os.MkdirTemp("", "bean-e2e")
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer teardown()
		if err := setup(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e setup:", err)
			return 1
		}
		if err := waitForCapacity(30 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "e2e setup:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

func build(name string) (string, error) {
	out := filepath.Join(binDir, name)
	cmd := exec.Command("go", "build", "-o", out, "github.com/garysng/bean/cmd/"+name)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %s", name, b)
	}
	return out, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func setup() error {
	agentBin, err := build("beand")
	if err != nil {
		return err
	}
	nodedBin, err := build("noded")
	if err != nil {
		return err
	}
	apiBin, err := build("bean-api")
	if err != nil {
		return err
	}
	cliBin, err = build("bean")
	if err != nil {
		return err
	}

	grpcPort, err := freePort()
	if err != nil {
		return err
	}
	httpPort, err := freePort()
	if err != nil {
		return err
	}
	nodeGRPCPort, err := freePort()
	if err != nil {
		return err
	}

	// The control plane starts first: nodes register outbound, so there is
	// nothing to register with until it is listening.
	api := exec.Command(apiBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--node-grpc", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
		"--region", "local",
		"--runtime-tier", "local",
		"--db", filepath.Join(binDir, "e2e.db"),
		"--api-key", apiKey,
		"--node-token", nodeToken)
	api.Stdout, api.Stderr = os.Stderr, os.Stderr
	if err := api.Start(); err != nil {
		return err
	}
	daemons = append(daemons, api)

	noded := exec.Command(nodedBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", grpcPort),
		"--runtime", "local",
		"--agent-bin", agentBin,
		"--base-dir", filepath.Join(binDir, "sandboxes"),
		"--node-token", nodeToken,
		"--control-plane", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
		"--node-id", "node-0",
		"--region", "local",
		"--advertise", fmt.Sprintf("127.0.0.1:%d", grpcPort),
		"--cpu", "16", "--memory-mib", "16384")
	noded.Stdout, noded.Stderr = os.Stderr, os.Stderr
	if err := noded.Start(); err != nil {
		return err
	}
	daemons = append(daemons, noded)

	apiURL = fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(apiURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bean-api not healthy")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForCapacity blocks until a node has registered and can host a
// sandbox, which is what "the cluster is ready" means now that nodes always
// register.
func waitForCapacity(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequest("GET", apiURL+"/v1/nodes", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var body struct {
				Nodes []struct {
					State string `json:"state"`
				} `json:"nodes"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			for _, n := range body.Nodes {
				if n.State == "READY" {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no ready node within %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func teardown() {
	for _, d := range daemons {
		if d.Process != nil {
			_ = d.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, d := range daemons {
		done := make(chan struct{})
		go func(c *exec.Cmd) { c.Wait(); close(done) }(d)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = d.Process.Kill()
		}
	}
	os.RemoveAll(binDir)
}

func api(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestE2EFullLoop(t *testing.T) {
	// 1. create
	code, out := api(t, "POST", "/v1/sandboxes", map[string]any{
		"image":  "python:3.12",
		"labels": map[string]string{"suite": "e2e"},
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["sandbox"].(map[string]any)["id"].(string)
	t.Logf("created %s", id)

	// 2. exec
	code, out = api(t, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo e2e-$((1+1))"},
	})
	if code != 200 || strings.TrimSpace(out["stdout"].(string)) != "e2e-2" {
		t.Fatalf("exec: %d %v", code, out)
	}

	// 3. file write via files API, read back via exec (shared sandbox root)
	req, _ := http.NewRequest("PUT", apiURL+"/v1/sandboxes/"+id+"/files?mkdirs=true&path=/w/data.txt",
		strings.NewReader("persisted"))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	fresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fresp.Body.Close()
	if fresp.StatusCode != 200 {
		t.Fatalf("file write: %d", fresp.StatusCode)
	}
	code, out = api(t, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"cat", "w/data.txt"}, // relative to sandbox root
	})
	if strings.TrimSpace(out["stdout"].(string)) != "persisted" {
		t.Fatalf("read back: %v", out)
	}

	// 4. pause -> exec wakes it
	code, _ = api(t, "POST", "/v1/sandboxes/"+id+"/pause", nil)
	if code != 202 {
		t.Fatalf("pause: %d", code)
	}
	code, out = api(t, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"echo", "awake"},
	})
	if code != 200 {
		t.Fatalf("exec-on-paused: %d %v", code, out)
	}

	// 5. events recorded
	code, out = api(t, "GET", "/v1/sandboxes/"+id+"/events", nil)
	types := map[string]bool{}
	for _, e := range out["events"].([]any) {
		types[e.(map[string]any)["type"].(string)] = true
	}
	for _, want := range []string{"sandbox.lifecycle.created", "sandbox.lifecycle.running", "sandbox.lifecycle.paused"} {
		if !types[want] {
			t.Errorf("missing event %s (have %v)", want, types)
		}
	}

	// 6. destroy
	code, _ = api(t, "DELETE", "/v1/sandboxes/"+id, nil)
	if code != 202 {
		t.Fatalf("delete: %d", code)
	}
	code, out = api(t, "GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "STOPPED" {
		t.Fatalf("post-delete state: %v", out)
	}
}

func TestE2EIdleDelete(t *testing.T) {
	code, out := api(t, "POST", "/v1/sandboxes", map[string]any{
		"image":     "x",
		"lifecycle": map[string]any{"idleTimeout": "1s", "onIdle": "delete"},
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["sandbox"].(map[string]any)["id"].(string)

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, out = api(t, "GET", "/v1/sandboxes/"+id, nil)
		state := out["sandbox"].(map[string]any)["state"].(string)
		if state == "STOPPED" || state == "FAILED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle delete did not happen, state=%s", state)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestE2EViaCLI(t *testing.T) {
	env := append(os.Environ(), "BEAN_BASE_URL="+apiURL, "BEAN_API_KEY="+apiKey)
	run := func(args ...string) (string, string, int) {
		cmd := exec.Command(cliBin, args...)
		cmd.Env = env
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return so.String(), se.String(), code
	}

	sout, serr, code := run("run", "--image", "busybox", "--label", "cli=e2e")
	if code != 0 {
		t.Fatalf("run: %s %s", sout, serr)
	}
	id := strings.Fields(sout)[0]

	sout, serr, code = run("exec", id, "--", "sh", "-c", "echo from-cli; exit 7")
	if code != 7 {
		t.Errorf("exec exit = %d (want 7), out=%q err=%q", code, sout, serr)
	}
	if !strings.Contains(sout, "from-cli") {
		t.Errorf("stdout = %q", sout)
	}

	// cp local -> sandbox -> local
	local := filepath.Join(t.TempDir(), "up.txt")
	os.WriteFile(local, []byte("cli-file"), 0o644)
	if _, serr, code = run("cp", local, "sbx:"+id+":/tmp/up.txt"); code != 0 {
		t.Fatalf("cp up: %s", serr)
	}
	down := filepath.Join(t.TempDir(), "down.txt")
	if _, serr, code = run("cp", "sbx:"+id+":/tmp/up.txt", down); code != 0 {
		t.Fatalf("cp down: %s", serr)
	}
	if b, _ := os.ReadFile(down); string(b) != "cli-file" {
		t.Errorf("roundtrip = %q", b)
	}

	sout, _, code = run("ls", "--label", "cli=e2e")
	if code != 0 || !strings.Contains(sout, id) {
		t.Errorf("ls: code=%d out=%q", code, sout)
	}

	if _, serr, code = run("kill", id); code != 0 {
		t.Fatalf("kill: %s", serr)
	}
}
