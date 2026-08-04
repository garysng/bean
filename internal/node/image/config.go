package image

// An image's configuration is metadata, not filesystem content, so converting an
// image to a bootable rootfs does not carry it: ENV, ENTRYPOINT, CMD, WORKDIR and
// USER live in the config blob, and flattening the layers into an ext4 leaves them
// behind. Something has to record them beside the image and apply them when a
// sandbox starts, or an image that depends on its own config -- most language
// runtimes -- behaves differently here than under `docker run`, silently.
//
// This file owns the second half of that: turning an image's recorded config plus
// what a caller asked for into the one process description the agent starts. The
// rules are OCI's rather than ours, and the reason they are written out as code
// with a table test is that the entrypoint/cmd interaction is where independent
// implementations usually diverge.

// Process is a fully resolved description of what to start in a sandbox: what to
// exec, in which directory, with which environment.
//
// Distinct from Config because a Config is what an image declares and a Process is
// what a particular sandbox runs. The merge between them is lossy -- a caller's Cmd
// replaces the image's -- so keeping one type for both would make "did this come
// from the image or the request" unanswerable at the point it matters.
type Process struct {
	// Argv is Entrypoint followed by Cmd, already joined. Empty means the image
	// declared no way to start and the caller supplied none, which is a sandbox
	// with nothing to run rather than an error: exec still works.
	Argv []string
	// Env is the full environment, image values overridden per key by the
	// caller's.
	Env map[string]string
	// Workdir is the directory to start in, "" to leave the agent's default.
	Workdir string
	// User is the image's USER as declared, unresolved. Carried through so the
	// agent can apply it once it can (see the User note in mergeConfig).
	User string
}

// MergeConfig resolves an image's config against what a caller requested.
//
// The rules are OCI's, and the one that matters most is that a caller's command
// replaces the image's Cmd while leaving Entrypoint in place. That is what makes
// `docker run python:3.12 -c 'print(1)'` pass arguments to the image's interpreter
// rather than trying to exec `-c` as a program. Overriding both together would
// look correct for images whose Entrypoint is empty -- which is most of the ones
// anyone tests with -- and break exactly the images that declare one.
//
// A nil cfg means the image has no recorded config: either it predates this being
// stored, or it is a build whose output carried none. The caller's request is then
// the whole answer, which is the behaviour every image had before configs were
// recorded.
func MergeConfig(cfg *Config, cmd []string, env map[string]string, workdir string) Process {
	var p Process

	if cfg == nil {
		p.Argv = append([]string{}, cmd...)
		p.Env = copyEnv(env)
		p.Workdir = workdir
		return p
	}

	// Entrypoint always survives; only Cmd is replaceable. An image with neither
	// contributes no argv, and the caller's cmd stands alone.
	argv := append([]string{}, cfg.Entrypoint...)
	if len(cmd) > 0 {
		argv = append(argv, cmd...)
	} else {
		argv = append(argv, cfg.Cmd...)
	}
	p.Argv = argv

	// The image's environment is the base and the caller's overrides it per key,
	// rather than replacing it wholesale: an image's PATH and a caller's one extra
	// variable both have to survive, and a caller cannot be expected to restate
	// the image's environment to add to it.
	p.Env = make(map[string]string, len(cfg.Env)+len(env))
	for _, kv := range cfg.Env {
		if k, v, ok := splitEnv(kv); ok {
			p.Env[k] = v
		}
	}
	for k, v := range env {
		p.Env[k] = v
	}

	p.Workdir = workdir
	if p.Workdir == "" {
		p.Workdir = cfg.WorkingDir
	}

	// Recorded but not yet enforced. Dropping privileges cannot happen where the
	// rest of this is applied: the agent is PID 1, so lowering its own uid would
	// cost it the ability to exec anything afterwards, and it has to be done in
	// the child instead. Resolving a name like "nobody" also needs the guest's
	// /etc/passwd, which only exists after the pivot. Carried through so the
	// value is available when that lands rather than being re-plumbed then.
	p.User = cfg.User

	return p
}

// splitEnv parses one "K=V" entry. An entry with no "=" is not a variable and is
// dropped rather than stored with an empty value: some images carry stray strings
// in Env, and inventing K="" for them would put a variable in the guest's
// environment that the image never declared.
func splitEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			if i == 0 {
				return "", "", false
			}
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}

func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}
