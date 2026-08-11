package cli

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNeedsContext(t *testing.T) {
	cases := map[string]bool{
		"FROM alpine\nRUN echo hi":           false,
		"FROM alpine\nCOPY . /app":           true,
		"FROM alpine\nADD x.tar /":           true,
		"from alpine\ncopy a b":              true, // case-insensitive
		"":                                   false,
		"FROM alpine\n\n   \n# COPY comment": false,
	}
	for df, want := range cases {
		if got := needsContext(df); got != want {
			t.Errorf("needsContext(%q) = %v, want %v", df, got, want)
		}
	}
}

func TestParseBuildArgs(t *testing.T) {
	if got := parseBuildArgs(""); got != nil {
		t.Errorf("empty = %v, want nil", got)
	}
	got := parseBuildArgs("A=1, B=two ,C=")
	if got["A"] != "1" || got["B"] != "two" || got["C"] != "" {
		t.Errorf("parseBuildArgs = %v", got)
	}
	// A pair with no '=' is dropped rather than becoming a keyless entry.
	if got := parseBuildArgs("novalue"); len(got) != 0 {
		t.Errorf("no-equals = %v, want empty", got)
	}
}

// TestPackContextRespectsIgnoreAndDockerfile packs a small tree and confirms the
// Dockerfile, .git and a .dockerignore'd path are excluded while a real file is
// carried.
func TestPackContextRespectsIgnoreAndDockerfile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM alpine\nCOPY . /app")
	write("app.go", "package main")
	write(".dockerignore", "secret.txt\n")
	write("secret.txt", "shh")
	write(".git/config", "[core]")

	tarball, err := packContext(dir, filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[hdr.Name] = true
	}
	if !names["app.go"] {
		t.Error("app.go missing from context")
	}
	for _, excluded := range []string{"Dockerfile", "secret.txt", ".git/config", ".dockerignore"} {
		if names[excluded] && excluded != ".dockerignore" {
			t.Errorf("%s should have been excluded", excluded)
		}
	}
}

// TestLoadDockerignoreDefaultsToGitOnly confirms that with no .dockerignore the
// predicate still excludes .git and nothing else.
func TestLoadDockerignoreDefaultsToGitOnly(t *testing.T) {
	dir := t.TempDir()
	ignore, err := loadDockerignore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ignore(".git") || !ignore(".git/config") {
		t.Error(".git not excluded by default")
	}
	if ignore("main.go") {
		t.Error("main.go excluded with no .dockerignore")
	}
}

// buildStub serves the build endpoints the CLI drives, recording request bodies.
func buildStub(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/templates/build", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		json.NewEncoder(w).Encode(map[string]any{"template": body["tag"], "state": "PENDING"})
	})
	mux.HandleFunc("GET /v1/templates/build/logs", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "step 1\nbuild succeeded\n")
	})
	mux.HandleFunc("POST /v1/templates/build/cancel", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"template": r.URL.Query().Get("ref"), "state": "CANCELLING"})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		json.NewEncoder(w).Encode(map[string]any{
			"sourceId": r.PathValue("id"),
			"sandboxes": []map[string]any{
				{"id": "sbx_fork1", "state": "RUNNING", "nodeId": "node-a"},
			},
			"failures": []map[string]any{
				{"index": 1, "code": "NO_CAPACITY", "message": "full"},
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &bodies
}

// TestCmdBuildRunOnly builds from a Dockerfile that needs no context and confirms
// no contextTar is uploaded.
func TestCmdBuildRunOnly(t *testing.T) {
	ts, bodies := buildStub(t)
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	os.WriteFile(df, []byte("FROM alpine\nRUN true"), 0o644)

	out, errStr, code := runCLI(t, ts, "build", "--tag", "app:v1", "--file", df, dir)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "app:v1") {
		t.Errorf("out = %q", out)
	}
	if len(*bodies) != 1 {
		t.Fatalf("bodies = %v", *bodies)
	}
	if _, packed := (*bodies)[0]["contextTar"]; packed {
		t.Error("a run-only build uploaded a context tar")
	}
}

// TestCmdBuildPacksContext builds from a COPY Dockerfile and confirms the context
// tar is uploaded and build args are threaded.
func TestCmdBuildPacksContext(t *testing.T) {
	ts, bodies := buildStub(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\nCOPY . /app"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("hi"), 0o644)

	_, errStr, code := runCLI(t, ts, "build", "--tag", "app:v2", "--build-arg", "K=V", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	body := (*bodies)[0]
	tarB64, packed := body["contextTar"].(string)
	if !packed {
		t.Fatal("context tar not uploaded for a COPY build")
	}
	if _, err := base64.StdEncoding.DecodeString(tarB64); err != nil {
		t.Errorf("context tar not base64: %v", err)
	}
	if args, ok := body["buildArgs"].(map[string]any); !ok || args["K"] != "V" {
		t.Errorf("buildArgs = %v", body["buildArgs"])
	}
}

func TestCmdBuildRequiresTag(t *testing.T) {
	ts, _ := buildStub(t)
	if _, _, code := runCLI(t, ts, "build"); code == 0 {
		t.Error("build with no --tag succeeded")
	}
}

func TestCmdBuildMissingDockerfile(t *testing.T) {
	ts, _ := buildStub(t)
	_, errStr, code := runCLI(t, ts, "build", "--tag", "x:v1", "--file", "/nonexistent/Dockerfile")
	if code == 0 || !strings.Contains(errStr, "read") {
		t.Errorf("code=%d err=%q", code, errStr)
	}
}

func TestCmdBuildLogsAndCancel(t *testing.T) {
	ts, _ := buildStub(t)
	out, errStr, code := runCLI(t, ts, "build", "logs", "app:v1")
	if code != 0 {
		t.Fatalf("logs code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "build succeeded") {
		t.Errorf("logs out = %q", out)
	}

	out, errStr, code = runCLI(t, ts, "build", "cancel", "app:v1")
	if code != 0 {
		t.Fatalf("cancel code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "CANCELLING") {
		t.Errorf("cancel out = %q", out)
	}

	// Both subcommands require a ref.
	if _, _, code := runCLI(t, ts, "build", "logs"); code == 0 {
		t.Error("build logs with no ref succeeded")
	}
	if _, _, code := runCLI(t, ts, "build", "cancel"); code == 0 {
		t.Error("build cancel with no ref succeeded")
	}
}

func TestCmdFork(t *testing.T) {
	ts, bodies := buildStub(t)
	out, errStr, code := runCLI(t, ts, "fork", "sbx_src", "--count", "2", "--label", "k=v")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "sbx_fork1") {
		t.Errorf("out = %q", out)
	}
	// The partial-failure note is surfaced, not swallowed.
	if !strings.Contains(out, "NO_CAPACITY") {
		t.Errorf("fork failure not reported: %q", out)
	}
	body := (*bodies)[0]
	if body["count"].(float64) != 2 {
		t.Errorf("count = %v", body["count"])
	}
	if lbls, ok := body["labels"].(map[string]any); !ok || lbls["k"] != "v" {
		t.Errorf("labels = %v", body["labels"])
	}
}

func TestCmdForkUsageAndBadCount(t *testing.T) {
	ts, _ := buildStub(t)
	if _, _, code := runCLI(t, ts, "fork"); code == 0 {
		t.Error("fork with no sandbox succeeded")
	}
	if _, _, code := runCLI(t, ts, "fork", "sbx_src", "--count", "0"); code == 0 {
		t.Error("fork with --count 0 succeeded")
	}
	if _, _, code := runCLI(t, ts, "fork", "sbx_src", "--count", "notnum"); code == 0 {
		t.Error("fork with non-numeric count succeeded")
	}
}
