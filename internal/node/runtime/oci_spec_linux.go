//go:build linux

package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// An OCI runtime takes a bundle: a rootfs directory plus config.json. This file
// writes that config.
//
// Hand-rolled rather than pulling in github.com/opencontainers/runtime-spec, because
// what is needed is one struct written once and never read back. The dependency would
// bring a large surface for a JSON document whose shape is fixed by the spec version
// named in it.
//
// The generated spec is deliberately close to what runc's own `spec` subcommand
// produces, so a difference in behaviour points at bean rather than at an unusual
// configuration.

// ociSpec is the subset of the OCI runtime spec bean sets. Fields it does not set are
// omitted rather than written empty, so the config reads as "these are the choices
// made" instead of burying them in defaults.
type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname,omitempty"`
	Mounts     []ociMount `json:"mounts"`
	Linux      *ociLinux  `json:"linux,omitempty"`
	Hooks      *ociHooks  `json:"hooks,omitempty"`
}

type ociProcess struct {
	Terminal        bool        `json:"terminal"`
	User            ociUser     `json:"user"`
	Args            []string    `json:"args"`
	Env             []string    `json:"env"`
	Cwd             string      `json:"cwd"`
	Capabilities    *ociCaps    `json:"capabilities,omitempty"`
	NoNewPrivileges bool        `json:"noNewPrivileges"`
	Rlimits         []ociRlimit `json:"rlimits,omitempty"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociCaps struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Permitted   []string `json:"permitted"`
	Inheritable []string `json:"inheritable,omitempty"`
	Ambient     []string `json:"ambient,omitempty"`
}

type ociRlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces  []ociNamespace `json:"namespaces"`
	Resources   *ociResources  `json:"resources,omitempty"`
	CgroupsPath string         `json:"cgroupsPath,omitempty"`
	// MaskedPaths and ReadonlyPaths hide host detail that a container has no reason
	// to see. Kept even under gVisor, whose /proc is its own implementation: the
	// container tier is meant to be able to run runc as well, where these are the
	// difference between a contained process and one reading host state.
	MaskedPaths   []string `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string `json:"readonlyPaths,omitempty"`
}

type ociNamespace struct {
	Type string `json:"type"`
	// Path joins an existing namespace instead of creating one. This is what makes
	// the network namespace bean's rather than the runtime's -- the node created it,
	// holds the host end of the veth, and dials the agent through it.
	Path string `json:"path,omitempty"`
}

type ociResources struct {
	Memory *ociMemory `json:"memory,omitempty"`
	CPU    *ociCPU    `json:"cpu,omitempty"`
	Pids   *ociPids   `json:"pids,omitempty"`
}

type ociMemory struct {
	Limit *int64 `json:"limit,omitempty"`
}

type ociCPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
}

type ociPids struct {
	Limit int64 `json:"limit"`
}

type ociHooks struct {
	CreateRuntime []ociHook `json:"createRuntime,omitempty"`
}

type ociHook struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// defaultCaps is what a sandbox process gets.
//
// This is runc's own default set minus CAP_NET_RAW. Dropping it costs a sandbox
// nothing it is expected to do -- ping and raw sockets are not why these exist -- and
// keeping it would let a sandbox forge packets on the namespace's veth, which is the
// one network the host shares with it.
//
// CAP_SYS_ADMIN is absent, which is what makes this not a privileged container: with
// it a process can mount, and mounting is most of the way out of a container.
var defaultCaps = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FSETID",
	"CAP_FOWNER",
	"CAP_MKNOD",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT",
	"CAP_KILL",
	"CAP_AUDIT_WRITE",
}

// maskedPaths are the host details a container should not read. Copied from runc's
// defaults: each entry is there because something in it identifies the host or its
// other tenants.
var maskedPaths = []string{
	"/proc/acpi",
	"/proc/asound",
	"/proc/kcore",
	"/proc/keys",
	"/proc/latency_stats",
	"/proc/timer_list",
	"/proc/timer_stats",
	"/proc/sched_debug",
	"/proc/scsi",
	"/sys/firmware",
	"/sys/devices/virtual/powercap",
}

var readonlyPaths = []string{
	"/proc/bus",
	"/proc/fs",
	"/proc/irq",
	"/proc/sys",
	"/proc/sysrq-trigger",
}

// bundleConfig is what a caller has to decide to produce a config.json.
type bundleConfig struct {
	// RootfsDir is the mounted image, an absolute path. The spec's root.path is
	// written relative to the bundle so the bundle stays movable.
	RootfsDir string
	// Args is the sandbox's entry process -- the agent, not the user's command. The
	// user's command is run through the agent afterwards, the same as on the fc tier.
	Args []string
	Env  []string
	Cwd  string
	// NetnsPath joins an existing network namespace. Empty leaves the runtime to
	// create one, which means the node cannot reach the agent -- supported only for
	// a node with no networking, where nothing could reach it anyway.
	NetnsPath string
	// AgentDir is bind-mounted in so the agent has somewhere to write state the node
	// reads. Empty skips the mount.
	AgentDir string
	// MemoryMiB, CPU and PidLimit are written as cgroup resources. Zero means
	// unlimited, matching what the fc tier does when no cgroup is configured.
	MemoryMiB int64
	CPU       float64
	PidLimit  int64
	// CgroupsPath is where the runtime should put the container's cgroup. Empty lets
	// the runtime choose.
	CgroupsPath string
}

// writeBundleConfig writes config.json into bundleDir.
func writeBundleConfig(bundleDir string, cfg bundleConfig) error {
	if cfg.RootfsDir == "" {
		return fmt.Errorf("runtime: bundle needs a rootfs directory")
	}
	if len(cfg.Args) == 0 {
		return fmt.Errorf("runtime: bundle needs an entry process")
	}

	// Relative to the bundle, because an OCI bundle is meant to be self-contained:
	// an absolute path here would break if the directory moved, and the runtime
	// resolves root.path against the bundle anyway.
	rootPath := cfg.RootfsDir
	if rel, err := filepath.Rel(bundleDir, cfg.RootfsDir); err == nil {
		rootPath = rel
	}

	spec := ociSpec{
		OCIVersion: "1.0.2",
		Process: ociProcess{
			Terminal: false,
			User:     ociUser{UID: 0, GID: 0},
			Args:     cfg.Args,
			Env:      cfg.Env,
			Cwd:      orDefault(cfg.Cwd, "/"),
			Capabilities: &ociCaps{
				Bounding:  defaultCaps,
				Effective: defaultCaps,
				Permitted: defaultCaps,
			},
			// The agent is the entry process and drops nothing further, so this only
			// closes the setuid route out of whatever the sandbox runs.
			NoNewPrivileges: true,
			Rlimits: []ociRlimit{
				{Type: "RLIMIT_NOFILE", Hard: 65536, Soft: 65536},
			},
		},
		Root:     ociRoot{Path: rootPath, Readonly: false},
		Hostname: "sandbox",
		Mounts:   defaultMounts(cfg.AgentDir),
		Linux: &ociLinux{
			Namespaces:    namespacesFor(cfg.NetnsPath),
			Resources:     resourcesFor(cfg),
			CgroupsPath:   cfg.CgroupsPath,
			MaskedPaths:   maskedPaths,
			ReadonlyPaths: readonlyPaths,
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode bundle config: %w", err)
	}
	path := filepath.Join(bundleDir, "config.json")
	// Written whole rather than streamed: a partial config.json is a runtime error
	// naming a JSON offset, which says nothing about the write that was cut short.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("runtime: write bundle config: %w", err)
	}
	return nil
}

// defaultMounts is the set every container needs to be a working Linux userspace.
//
// /sys is read-only rather than absent: a missing /sys breaks tools that read it
// (including parts of the Go runtime's CPU detection), while a writable one lets a
// container reconfigure host devices.
func defaultMounts(agentDir string) []ociMount {
	m := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts",
			Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm",
			Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue",
			Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{"nosuid", "noexec", "nodev", "ro"}},
	}
	if agentDir != "" {
		// rbind rather than bind, so anything mounted underneath comes with it. rw
		// because the agent writes here and the node reads what it wrote.
		m = append(m, ociMount{
			Destination: "/run/bean", Type: "bind", Source: agentDir,
			Options: []string{"rbind", "rw", "nosuid", "nodev"},
		})
	}
	return m
}

// namespacesFor lists the namespaces the container gets.
//
// The network one carries a path when the node made a namespace for this sandbox,
// which is the arrangement the agent transport depends on: the node holds the host
// end of the veth and dials in. Without a path the runtime creates its own, which
// isolates the container but leaves nothing able to reach it.
func namespacesFor(netnsPath string) []ociNamespace {
	ns := []ociNamespace{
		{Type: "pid"},
		{Type: "ipc"},
		{Type: "uts"},
		{Type: "mount"},
		{Type: "cgroup"},
	}
	ns = append(ns, ociNamespace{Type: "network", Path: netnsPath})
	return ns
}

func resourcesFor(cfg bundleConfig) *ociResources {
	var res ociResources
	set := false
	if cfg.MemoryMiB > 0 {
		limit := cfg.MemoryMiB << 20
		res.Memory = &ociMemory{Limit: &limit}
		set = true
	}
	if cfg.CPU > 0 {
		// 100ms is the conventional period; the quota is the share of one period the
		// container may use, so quota/period is the CPU count.
		period := uint64(100000)
		quota := int64(cfg.CPU * float64(period))
		res.CPU = &ociCPU{Quota: &quota, Period: &period}
		set = true
	}
	if cfg.PidLimit > 0 {
		res.Pids = &ociPids{Limit: cfg.PidLimit}
		set = true
	}
	if !set {
		return nil
	}
	return &res
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
