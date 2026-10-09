package cirunner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeJIT = "ZmFrZS1qaXQtY29uZmln" // base64 of a fake config; a bearer credential in reality

// fake serves both the GitHub and bean endpoints the controller calls, records each call
// in order, and lets a test make any one of them fail.
type fake struct {
	mu        sync.Mutex
	calls     []string
	execBody  map[string]any
	create    map[string]any
	sandboxes []Sandbox
	runners   []Runner
	fail      map[string]int // "METHOD prefix" -> status to return
	execBlock chan struct{}  // when set, exec waits on it (or on the request ending)
}

func (f *fake) record(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fake) failFor(r *http.Request) int {
	for k, code := range f.fail {
		m, prefix, _ := strings.Cut(k, " ")
		if r.Method == m && strings.HasPrefix(r.URL.Path, prefix) {
			return code
		}
	}
	return 0
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.record(key)
	if code := f.failFor(r); code != 0 {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"message":"injected","error":{"code":"INJECTED","message":"injected"}}`)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/generate-jitconfig"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner": map[string]any{"id": 42, "name": body["name"]}, "encoded_jit_config": fakeJIT})
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/actions/runners/"):
		// A JIT runner that finished its job has already deregistered itself.
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runners"):
		_ = json.NewEncoder(w).Encode(map[string]any{"runners": f.runners})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		f.mu.Lock()
		f.create = body
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"sandbox": map[string]any{"id": "sbx-1", "state": "running"}})
	case strings.HasSuffix(r.URL.Path, "/exec"):
		f.mu.Lock()
		f.execBody = body
		block := f.execBlock
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "Job completed"})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": f.sandboxes})
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func (f *fake) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newController(t *testing.T, f *fake) *Controller {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Controller{
		Config: Config{Repo: "garysng/maodou", Labels: []string{"self-hosted", "bean"},
			Image: "ghcr.io/actions/actions-runner:latest", JobTimeout: time.Minute, IdleTimeout: 2 * time.Minute},
		GitHub:  &GitHub{APIURL: srv.URL, Token: "t", HTTP: srv.Client()},
		Bean:    &Bean{BaseURL: srv.URL, HTTP: srv.Client()},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewName: func() string { return "bean-ci-test" },
	}
}

func TestRunOnceProvisionsRunsAndCleansUpInOrder(t *testing.T) {
	f := &fake{}
	code, err := newController(t, f).RunOnce(context.Background())
	if err != nil || code != 0 {
		t.Fatalf("RunOnce = %d, %v", code, err)
	}
	want := []string{
		"POST /repos/garysng/maodou/actions/runners/generate-jitconfig",
		"POST /v1/sandboxes",
		"POST /v1/sandboxes/sbx-1/exec",
		"DELETE /v1/sandboxes/sbx-1",
		// Called even on success, and its 404 is not an error: a JIT runner removes
		// itself, and this is the only way to know it did.
		"DELETE /repos/garysng/maodou/actions/runners/42",
	}
	if got := f.callList(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheJITConfigTravelsInEnvNeverOnTheCommandLine(t *testing.T) {
	f := &fake{}
	if _, err := newController(t, f).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd, _ := json.Marshal(f.execBody["cmd"])
	if strings.Contains(string(cmd), fakeJIT) {
		t.Fatalf("JIT config is on the command line, readable from /proc/<pid>/cmdline: %s", cmd)
	}
	env, _ := f.execBody["env"].(map[string]any)
	if env["ACTIONS_RUNNER_INPUT_JITCONFIG"] != fakeJIT {
		t.Fatalf("JIT config did not reach the runner's env: %v", env)
	}
}

func TestTheSandboxIsLabelledAndHasAnIdleBackstop(t *testing.T) {
	f := &fake{}
	if _, err := newController(t, f).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	labels, _ := f.create["labels"].(map[string]any)
	if labels[LabelPurpose] != PurposeValue || labels[LabelRunner] != "bean-ci-test" || labels[LabelRepo] != "garysng/maodou" {
		t.Fatalf("labels = %v", labels)
	}
	lc, _ := f.create["lifecycle"].(map[string]any)
	if lc["idleTimeout"] != "120s" || lc["onIdle"] != "delete" {
		t.Fatalf("lifecycle = %v: a controller killed mid-job would leak this sandbox", lc)
	}
	if f.create["imageRef"] != "ghcr.io/actions/actions-runner:latest" {
		t.Fatalf("imageRef = %v", f.create["imageRef"])
	}
}

func TestAFailedCreateStillRemovesTheRegisteredRunner(t *testing.T) {
	f := &fake{fail: map[string]int{"POST /v1/sandboxes": 503}}
	if _, err := newController(t, f).RunOnce(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	calls := f.callList()
	if last := calls[len(calls)-1]; last != "DELETE /repos/garysng/maodou/actions/runners/42" {
		t.Fatalf("runner not removed after create failed; calls = %v", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE /v1/sandboxes") {
			t.Fatalf("deleted a sandbox that was never created: %v", calls)
		}
	}
}

func TestAFailedExecDeletesTheSandboxAndTheRunner(t *testing.T) {
	f := &fake{fail: map[string]int{"POST /v1/sandboxes/sbx-1/exec": 500}}
	if _, err := newController(t, f).RunOnce(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	got := strings.Join(f.callList(), "\n")
	for _, want := range []string{"DELETE /v1/sandboxes/sbx-1", "DELETE /repos/garysng/maodou/actions/runners/42"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q after exec failed; calls:\n%s", want, got)
		}
	}
}

func TestCancellationMidJobStillCleansUp(t *testing.T) {
	// The common reason to stop is SIGTERM to the controller. Cleanup that inherited the
	// cancelled context would fail at once and leave both resources behind.
	f := &fake{execBlock: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := newController(t, f).RunOnce(ctx); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(f.callList(), " "), "/exec") {
		if time.Now().After(deadline) {
			t.Fatal("exec never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	got := strings.Join(f.callList(), "\n")
	for _, want := range []string{"DELETE /v1/sandboxes/sbx-1", "DELETE /repos/garysng/maodou/actions/runners/42"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q after cancellation; calls:\n%s", want, got)
		}
	}
}

func TestReapRemovesOnlyWhatACrashedControllerLeft(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f := &fake{
		sandboxes: []Sandbox{
			{ID: "old", CreatedAt: now.Add(-3 * time.Hour), Labels: map[string]string{LabelRunner: "bean-ci-old"}},
			{ID: "young", CreatedAt: now.Add(-time.Minute), Labels: map[string]string{LabelRunner: "bean-ci-starting"}},
			{ID: "undated", Labels: map[string]string{LabelRunner: "bean-ci-undated"}},
		},
		runners: []Runner{
			{ID: 1, Name: "bean-ci-old", Status: "offline"},      // its sandbox is being reaped
			{ID: 2, Name: "bean-ci-starting", Status: "offline"}, // offline only because it is still starting
			{ID: 3, Name: "bean-ci-orphan", Status: "offline"},   // no sandbox at all
			{ID: 4, Name: "bean-ci-working", Status: "online"},   // never touched
			{ID: 5, Name: "bean-ci-busy", Status: "offline", Busy: true},
			{ID: 6, Name: "someone-elses", Status: "offline"}, // not ours
		},
	}
	c := newController(t, f)
	c.Now = func() time.Time { return now }

	dry, err := c.Reap(context.Background(), 2*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.callList() {
		if strings.HasPrefix(call, "DELETE") {
			t.Fatalf("dry run deleted something: %s", call)
		}
	}
	if got := strings.Join(dry.Sandboxes, ","); got != "old,undated" {
		t.Fatalf("sandboxes = %s, want old,undated", got)
	}
	if got := strings.Join(dry.Runners, ","); got != "bean-ci-old#1,bean-ci-orphan#3" {
		t.Fatalf("runners = %s, want bean-ci-old#1,bean-ci-orphan#3 (a starting runner must survive)", got)
	}

	if _, err := c.Reap(context.Background(), 2*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.callList(), "\n")
	for _, want := range []string{"DELETE /v1/sandboxes/old", "DELETE /v1/sandboxes/undated",
		"DELETE /repos/garysng/maodou/actions/runners/1", "DELETE /repos/garysng/maodou/actions/runners/3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	for _, never := range []string{"DELETE /v1/sandboxes/young",
		"DELETE /repos/garysng/maodou/actions/runners/2", "DELETE /repos/garysng/maodou/actions/runners/4",
		"DELETE /repos/garysng/maodou/actions/runners/5", "DELETE /repos/garysng/maodou/actions/runners/6"} {
		for _, call := range f.callList() {
			if call == never {
				t.Fatalf("%s: not abandoned, must not be removed", never)
			}
		}
	}
}

func TestGenerateJITRejectsAResponseItCannotClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"runner":{"id":0},"encoded_jit_config":"x"}`)
	}))
	defer srv.Close()
	g := &GitHub{APIURL: srv.URL, HTTP: srv.Client()}
	if _, err := g.GenerateJIT(context.Background(), "a/b", "n", []string{"x"}); err == nil {
		t.Fatal("a runner with no id cannot be removed later; it must be refused")
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Repo: "a/b", Labels: []string{"bean"}, Image: "i", JobTimeout: time.Minute, IdleTimeout: time.Hour}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"repo without owner":            func(c *Config) { c.Repo = "maodou" },
		"no labels":                     func(c *Config) { c.Labels = nil },
		"no image":                      func(c *Config) { c.Image = "" },
		"job past bean's exec ceiling":  func(c *Config) { c.JobTimeout = time.Hour },
		"idle timeout shorter than job": func(c *Config) { c.IdleTimeout = 30 * time.Second },
	} {
		c := ok
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
