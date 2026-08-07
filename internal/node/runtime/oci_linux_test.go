//go:build linux

package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Nothing else checks this at compile time, and the mismatch it catches is not
// obvious: an implementation can be complete, compile, and still not satisfy the
// interface because one parameter is a structurally similar but different type. This
// caught Checkpoint taking an inline interface instead of io.Writer.
var _ Runtime = (*OCIRuntime)(nil)

// The bundle config is the whole contract with the runtime binary, and a wrong field
// surfaces as a container that does not start with an error naming a JSON path. So the
// properties that matter are asserted on the written file rather than on the struct.

func readSpec(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("config.json is not valid JSON: %v", err)
	}
	return spec
}

// The network namespace must carry a path, because that is what makes it the node's
// namespace rather than one the runtime created. Without it the container is isolated
// and unreachable -- a sandbox that starts and cannot be talked to.
func TestBundleJoinsTheNodesNetworkNamespace(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleConfig(dir, bundleConfig{
		RootfsDir: rootfs,
		Args:      []string{"/.bean/beand", "--listen", "0.0.0.0:8111"},
		NetnsPath: "/var/run/netns/bean-7",
	}); err != nil {
		t.Fatalf("writeBundleConfig: %v", err)
	}

	spec := readSpec(t, dir)
	linux := spec["linux"].(map[string]any)
	var netPath string
	found := false
	for _, n := range linux["namespaces"].([]any) {
		ns := n.(map[string]any)
		if ns["type"] == "network" {
			found = true
			netPath, _ = ns["path"].(string)
		}
	}
	if !found {
		t.Fatal("no network namespace in the spec")
	}
	if netPath != "/var/run/netns/bean-7" {
		t.Errorf("network namespace path = %q, want the node's namespace", netPath)
	}
}

// A root path relative to the bundle, not absolute: an OCI bundle is meant to be
// self-contained, and the runtime resolves root.path against the bundle anyway.
func TestBundleRootPathIsRelative(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleConfig(dir, bundleConfig{
		RootfsDir: rootfs,
		Args:      []string{"/.bean/beand"},
		NetnsPath: "/var/run/netns/bean-1",
	}); err != nil {
		t.Fatal(err)
	}
	root := readSpec(t, dir)["root"].(map[string]any)
	if got := root["path"].(string); got != "rootfs" {
		t.Errorf("root.path = %q, want %q", got, "rootfs")
	}
}

// The capability set is a security boundary, so the two absences that define it are
// asserted rather than left to review of a list.
//
// CAP_SYS_ADMIN would let a process mount, which is most of the way out of a
// container. CAP_NET_RAW would let it forge packets on the veth it shares with the
// host, which is the one network the host exposes to it.
func TestBundleDropsTheCapabilitiesThatMatter(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleConfig(dir, bundleConfig{
		RootfsDir: rootfs,
		Args:      []string{"/.bean/beand"},
		NetnsPath: "/var/run/netns/bean-1",
	}); err != nil {
		t.Fatal(err)
	}

	proc := readSpec(t, dir)["process"].(map[string]any)
	caps := proc["capabilities"].(map[string]any)
	for _, set := range []string{"bounding", "effective", "permitted"} {
		list := caps[set].([]any)
		for _, c := range list {
			switch c.(string) {
			case "CAP_SYS_ADMIN":
				t.Errorf("%s includes CAP_SYS_ADMIN: a process with it can mount", set)
			case "CAP_NET_RAW":
				t.Errorf("%s includes CAP_NET_RAW: a process with it can forge packets "+
					"on the veth shared with the host", set)
			}
		}
		if len(list) == 0 {
			t.Errorf("%s is empty; the sandbox could not chown or setuid", set)
		}
	}
	if proc["noNewPrivileges"] != true {
		t.Error("noNewPrivileges is not set; setuid binaries stay a route out")
	}
}

// Resources are only written when asked for. A zero limit written as a limit would cap
// a sandbox at nothing, so absence has to mean unlimited.
func TestBundleOmitsResourcesWhenUnbounded(t *testing.T) {
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleConfig(dir, bundleConfig{
		RootfsDir: rootfs,
		Args:      []string{"/.bean/beand"},
		NetnsPath: "/var/run/netns/bean-1",
	}); err != nil {
		t.Fatal(err)
	}
	linux := readSpec(t, dir)["linux"].(map[string]any)
	if _, ok := linux["resources"]; ok {
		t.Error("resources written for an unbounded sandbox")
	}

	// And present when they are asked for, or a spec'd limit would silently not apply.
	dir2 := t.TempDir()
	rootfs2 := filepath.Join(dir2, "rootfs")
	if err := os.MkdirAll(rootfs2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundleConfig(dir2, bundleConfig{
		RootfsDir: rootfs2,
		Args:      []string{"/.bean/beand"},
		NetnsPath: "/var/run/netns/bean-1",
		MemoryMiB: 512,
		CPU:       2,
	}); err != nil {
		t.Fatal(err)
	}
	res := readSpec(t, dir2)["linux"].(map[string]any)["resources"].(map[string]any)
	if got := res["memory"].(map[string]any)["limit"].(float64); got != 512<<20 {
		t.Errorf("memory limit = %v, want %v bytes", got, 512<<20)
	}
	cpu := res["cpu"].(map[string]any)
	// quota/period is the CPU count, so 2 CPUs at the conventional 100ms period.
	if q, p := cpu["quota"].(float64), cpu["period"].(float64); q/p != 2 {
		t.Errorf("cpu quota/period = %v/%v, want a ratio of 2", q, p)
	}
}

// A bundle with no entry process or no rootfs is refused rather than written: the
// runtime's own error for either is about a missing field, which does not say that the
// caller never supplied one.
func TestBundleRefusesIncompleteConfig(t *testing.T) {
	dir := t.TempDir()
	if err := writeBundleConfig(dir, bundleConfig{Args: []string{"x"}}); err == nil {
		t.Error("accepted a bundle with no rootfs")
	}
	if err := writeBundleConfig(dir, bundleConfig{RootfsDir: dir}); err == nil {
		t.Error("accepted a bundle with no entry process")
	}
}

// PATH is supplied when the caller sets none. Without it a container cannot resolve a
// bare command name, which surfaces as "executable file not found" for a binary that
// is plainly present.
func TestEnvAlwaysCarriesAPath(t *testing.T) {
	got := strings.Join(envList(map[string]string{"FOO": "bar"}), " ")
	if !strings.Contains(got, "PATH=") {
		t.Errorf("no PATH in %q", got)
	}
	// And the caller's own PATH is not duplicated or overridden.
	got2 := envList(map[string]string{"PATH": "/only/here"})
	if len(got2) != 1 || got2[0] != "PATH=/only/here" {
		t.Errorf("caller's PATH not respected: %v", got2)
	}
}

// Checkpoint has to fail in a way the scheduler can distinguish from a transient
// error: the first means place the work elsewhere, the second means retry.
func TestCheckpointReportsUnsupportedDistinctly(t *testing.T) {
	r := NewOCIRuntime("runsc", "/bin/true", t.TempDir(), nil)
	err := r.Checkpoint(nil, "sbx", os.Stdout, CheckpointOptions{})
	if err == nil {
		t.Fatal("Checkpoint claimed to succeed")
	}
	if !strings.Contains(err.Error(), "cannot checkpoint") {
		t.Errorf("error does not say the tier cannot checkpoint: %v", err)
	}
	if _, ferr := r.Fork(nil, &Spec{SandboxID: "sbx"}, nil); ferr == nil {
		t.Error("Fork claimed to succeed")
	}
}
