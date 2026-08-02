//go:build unix

package beand

import (
	"os"
	"os/exec"
	"syscall"
)

// execSysProcAttr puts the child in its own process group so the whole
// tree can be signalled on timeout.
func execSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup terminates the child's entire process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// baseEnvAllowlist is the set of host environment variables propagated to
// user processes. Everything else (potential platform secrets) is dropped.
var baseEnvAllowlist = []string{"PATH", "HOME", "TERM", "LANG", "LC_ALL", "TZ"}

// DefaultPath is the search path for user commands.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// EnsurePath gives the agent a search path when it started without one.
//
// The kernel hands PID 1 an empty environment, so in a microVM the agent has no
// PATH at all. That breaks resolving a bare command name like "echo": the lookup
// runs in the agent's own environment, not the one assembled for the child, so
// setting PATH only for children is not enough.
func EnsurePath() {
	if _, ok := os.LookupEnv("PATH"); !ok {
		_ = os.Setenv("PATH", DefaultPath)
	}
}

// buildEnv assembles the child environment: an allowlisted subset of the
// host env plus the caller-provided variables (which win on conflict).
func buildEnv(extra map[string]string) []string {
	env := make([]string, 0, len(baseEnvAllowlist)+len(extra))
	for _, k := range baseEnvAllowlist {
		if _, ok := extra[k]; ok {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
