package image

import (
	"reflect"
	"testing"
)

// The cases below are the documented `docker run` semantics, and they are written
// out one by one because the entrypoint/cmd interaction is the part independent
// implementations get wrong. The specific trap: overriding Cmd and Entrypoint
// together looks correct for every image whose Entrypoint is empty -- which is most
// of what anyone reaches for while testing -- and breaks exactly the images that
// declare one.
func TestMergeConfigFollowsDockerRunSemantics(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		cmd     []string
		env     map[string]string
		workdir string
		want    Process
	}{
		{
			name: "image cmd runs when the caller asks for nothing",
			cfg:  &Config{Cmd: []string{"nginx", "-g", "daemon off;"}},
			want: Process{Argv: []string{"nginx", "-g", "daemon off;"}, Env: map[string]string{}},
		},
		{
			// The rule that matters most: `docker run python:3.12 -c 'print(1)'`
			// passes the argument to the interpreter rather than trying to exec "-c".
			name: "caller cmd replaces image cmd but keeps entrypoint",
			cfg: &Config{
				Entrypoint: []string{"python3"},
				Cmd:        []string{"-i"},
			},
			cmd:  []string{"-c", "print(1)"},
			want: Process{Argv: []string{"python3", "-c", "print(1)"}, Env: map[string]string{}},
		},
		{
			name: "image entrypoint and cmd concatenate when the caller is silent",
			cfg: &Config{
				Entrypoint: []string{"/bin/sh", "-c"},
				Cmd:        []string{"echo hi"},
			},
			want: Process{Argv: []string{"/bin/sh", "-c", "echo hi"}, Env: map[string]string{}},
		},
		{
			name: "caller cmd stands alone when the image declares no entrypoint",
			cfg:  &Config{Cmd: []string{"sh"}},
			cmd:  []string{"ls", "-la"},
			want: Process{Argv: []string{"ls", "-la"}, Env: map[string]string{}},
		},
		{
			name: "image env is the base and the caller overrides per key",
			cfg: &Config{
				Env: []string{"PATH=/usr/local/bin:/usr/bin", "LANG=C.UTF-8", "PYTHONPATH=/app"},
				Cmd: []string{"python3"},
			},
			env: map[string]string{"LANG": "en_US.UTF-8", "EXTRA": "1"},
			want: Process{
				Argv: []string{"python3"},
				Env: map[string]string{
					"PATH":       "/usr/local/bin:/usr/bin",
					"LANG":       "en_US.UTF-8",
					"PYTHONPATH": "/app",
					"EXTRA":      "1",
				},
			},
		},
		{
			name: "image workdir applies when the caller names none",
			cfg:  &Config{WorkingDir: "/testbed", Cmd: []string{"pytest"}},
			want: Process{Argv: []string{"pytest"}, Env: map[string]string{}, Workdir: "/testbed"},
		},
		{
			name:    "caller workdir wins over the image's",
			cfg:     &Config{WorkingDir: "/testbed", Cmd: []string{"pytest"}},
			workdir: "/src",
			want:    Process{Argv: []string{"pytest"}, Env: map[string]string{}, Workdir: "/src"},
		},
		{
			// A value carried through rather than applied: dropping privileges needs
			// fork-then-setuid in the child and the guest's own /etc/passwd, neither of
			// which exists where the merge happens.
			name: "user is carried through unresolved",
			cfg:  &Config{User: "nobody", Cmd: []string{"true"}},
			want: Process{Argv: []string{"true"}, Env: map[string]string{}, User: "nobody"},
		},
		{
			// An image predating config recording, or a build's output. The request is
			// then the whole answer, which is what every image did before.
			name: "a nil config leaves the request untouched",
			cfg:  nil,
			cmd:  []string{"echo", "hi"},
			env:  map[string]string{"A": "1"},
			want: Process{Argv: []string{"echo", "hi"}, Env: map[string]string{"A": "1"}},
		},
		{
			name: "nothing to run is not an error",
			cfg:  &Config{},
			want: Process{Argv: []string{}, Env: map[string]string{}},
		},
		{
			// Some images carry stray non-assignments in Env. Storing them as K=""
			// would put a variable in the guest that the image never declared.
			name: "env entries without a separator are dropped",
			cfg:  &Config{Env: []string{"VALID=1", "JUNK", "=noname"}, Cmd: []string{"true"}},
			want: Process{Argv: []string{"true"}, Env: map[string]string{"VALID": "1"}},
		},
		{
			name: "an env value may itself contain the separator",
			cfg:  &Config{Env: []string{"GOFLAGS=-ldflags=-s -w"}, Cmd: []string{"true"}},
			want: Process{Argv: []string{"true"}, Env: map[string]string{"GOFLAGS": "-ldflags=-s -w"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeConfig(tc.cfg, tc.cmd, tc.env, tc.workdir)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MergeConfig()\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A config has to survive the sidecar round trip, since that is the only thing
// carrying it from conversion (where the registry is reachable) to create (where it
// is not).
func TestConfigSurvivesTheSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Config{
		Env:        []string{"PATH=/usr/bin", "LANG=C.UTF-8"},
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-i"},
		WorkingDir: "/testbed",
		User:       "nobody",
	}

	if err := recordRef(dir, "python:3.12", "sha256:abc", want); err != nil {
		t.Fatalf("recordRef: %v", err)
	}

	got, err := cachedConfig(dir, "python:3.12")
	if err != nil {
		t.Fatalf("cachedConfig: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cachedConfig()\n got %+v\nwant %+v", got, want)
	}

	// The digest must still be readable: config and digest share one sidecar, and
	// writing one must not displace the other.
	digest, err := cachedDigest(dir, "python:3.12")
	if err != nil || digest != "sha256:abc" {
		t.Errorf("cachedDigest() = %q, %v; want sha256:abc", digest, err)
	}
}

// An image with no recorded config must read back as nil rather than as a
// zero-valued Config, because the two mean different things to a create: nil says
// "start from the request alone", while an empty Config would claim the image
// genuinely declares no entrypoint.
func TestCachedConfigIsNilWhenNoneWasRecorded(t *testing.T) {
	dir := t.TempDir()
	if err := recordRef(dir, "built:1", "", nil); err != nil {
		t.Fatalf("recordRef: %v", err)
	}

	got, err := cachedConfig(dir, "built:1")
	if err != nil {
		t.Fatalf("cachedConfig: %v", err)
	}
	if got != nil {
		t.Errorf("cachedConfig() = %+v, want nil", got)
	}
}

// An image this node has never seen is a miss, not a failure: the caller's next
// step is to convert it.
func TestCachedConfigIsNilForAnUnknownImage(t *testing.T) {
	got, err := cachedConfig(t.TempDir(), "absent:1")
	if err != nil {
		t.Fatalf("cachedConfig: %v", err)
	}
	if got != nil {
		t.Errorf("cachedConfig() = %+v, want nil", got)
	}
}

// The merge must not write through to the image's recorded config: it is read from
// a cache that other sandboxes on this node share, so a mutation would leak one
// sandbox's environment into the next create from the same image.
func TestMergeConfigDoesNotMutateTheImageConfig(t *testing.T) {
	cfg := &Config{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-i"},
		Env:        []string{"LANG=C.UTF-8"},
	}

	got := MergeConfig(cfg, []string{"-c", "print(1)"}, map[string]string{"LANG": "en_US"}, "")
	got.Argv[0] = "mutated"

	if cfg.Entrypoint[0] != "python3" {
		t.Errorf("Entrypoint = %q, want it untouched", cfg.Entrypoint[0])
	}
	if !reflect.DeepEqual(cfg.Cmd, []string{"-i"}) {
		t.Errorf("Cmd = %v, want it untouched", cfg.Cmd)
	}
	if !reflect.DeepEqual(cfg.Env, []string{"LANG=C.UTF-8"}) {
		t.Errorf("Env = %v, want it untouched", cfg.Env)
	}
}
