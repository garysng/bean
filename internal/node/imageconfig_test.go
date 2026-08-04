package node

import (
	"context"
	"reflect"
	"testing"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/runtime"
)

// configRuntime wraps the local runtime with an image configuration, so the merge
// the manager performs can be observed without a registry or a microVM.
//
// It embeds rather than reimplements Runtime: the point under test is which fields
// reach the agent, and every other behaviour should be the real one.
type configRuntime struct {
	*runtime.LocalRuntime
	cfg *image.Config
	err error
}

func (r *configRuntime) ImageConfig(string) (*image.Config, error) {
	return r.cfg, r.err
}

// resolveProcess is the merge as the manager performs it. Exercising it through the
// manager rather than calling image.MergeConfig directly is the point: the bug this
// covers was not a wrong merge but a merge that was never invoked, with the image's
// Entrypoint and Workdir left out of the request entirely.
func TestResolveProcessAppliesTheImageConfig(t *testing.T) {
	rt := &configRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		cfg: &image.Config{
			Env:        []string{"PATH=/opt/conda/bin", "LANG=C.UTF-8"},
			Entrypoint: []string{"python3"},
			Cmd:        []string{"-i"},
			WorkingDir: "/testbed",
		},
	}
	m := NewManager(rt)
	t.Cleanup(m.Close)

	got, err := m.resolveProcess(spec("sbx-cfg", func(s *nodev1.SandboxSpec) {
		s.Cmd = []string{"-c", "print(1)"}
		s.Env = map[string]string{"LANG": "en_US.UTF-8"}
	}))
	if err != nil {
		t.Fatalf("resolveProcess: %v", err)
	}

	want := image.Process{
		Argv:    []string{"python3", "-c", "print(1)"},
		Env:     map[string]string{"PATH": "/opt/conda/bin", "LANG": "en_US.UTF-8"},
		Workdir: "/testbed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveProcess()\n got %+v\nwant %+v", got, want)
	}
}

// A runtime that cannot report configs -- the local dev tier is one -- must behave
// as it did before configs were recorded, using the request alone.
func TestResolveProcessFallsBackToTheRequest(t *testing.T) {
	m := newTestManager(t)

	got, err := m.resolveProcess(spec("sbx-plain", func(s *nodev1.SandboxSpec) {
		s.Cmd = []string{"echo", "hi"}
		s.Env = map[string]string{"A": "1"}
	}))
	if err != nil {
		t.Fatalf("resolveProcess: %v", err)
	}

	want := image.Process{Argv: []string{"echo", "hi"}, Env: map[string]string{"A": "1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveProcess()\n got %+v\nwant %+v", got, want)
	}
}

// An image with no recorded config yields the request, not an empty process: this is
// what a build's output looks like, and it has to remain startable.
func TestResolveProcessHandlesAnImageWithNoConfig(t *testing.T) {
	rt := &configRuntime{LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir())}
	m := NewManager(rt)
	t.Cleanup(m.Close)

	got, err := m.resolveProcess(spec("sbx-noconfig", func(s *nodev1.SandboxSpec) {
		s.Cmd = []string{"true"}
	}))
	if err != nil {
		t.Fatalf("resolveProcess: %v", err)
	}
	if !reflect.DeepEqual(got.Argv, []string{"true"}) {
		t.Errorf("Argv = %v, want [true]", got.Argv)
	}
}

// autoStartCmd starts the image's own entrypoint even when the request names no
// command. Before the config was wired through, a create like this started nothing
// at all -- the guard was on the request's Cmd being non-empty.
func TestAutoStartRunsTheImageEntrypointWithNoRequestCmd(t *testing.T) {
	rt := &configRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		cfg:          &image.Config{Entrypoint: []string{"sleep"}, Cmd: []string{"30"}},
	}
	m := NewManager(rt)
	t.Cleanup(m.Close)

	got, err := m.resolveProcess(spec("sbx-auto"))
	if err != nil {
		t.Fatalf("resolveProcess: %v", err)
	}
	if !reflect.DeepEqual(got.Argv, []string{"sleep", "30"}) {
		t.Errorf("Argv = %v, want [sleep 30]", got.Argv)
	}
}

// A create must survive an unreadable config. The sandbox is already running by the
// time the user process is started, so failing here would destroy a working sandbox
// over metadata.
func TestCreateSucceedsWhenTheImageConfigCannotBeRead(t *testing.T) {
	rt := &configRuntime{
		LocalRuntime: runtime.NewLocalRuntime(agentBin, t.TempDir()),
		err:          context.DeadlineExceeded,
	}
	m := NewManager(rt)
	t.Cleanup(m.Close)

	sb, err := m.Create(context.Background(), spec("sbx-badcfg", func(s *nodev1.SandboxSpec) {
		s.AutoStartCmd = true
		s.Cmd = []string{"true"}
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.State != runtime.StateRunning {
		t.Errorf("state = %s, want RUNNING", sb.State)
	}
}
