//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Firecracker is configured over a Unix-socket HTTP API. A config file could
// describe the initial machine, but pause, resume and snapshot are only
// reachable through the API, so one client covers the whole lifecycle.

// fcClient talks to one microVM's API socket.
type fcClient struct {
	http *http.Client
}

func newFCClient(apiSocket string) *fcClient {
	return &fcClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", apiSocket)
				},
			},
			// Snapshot creation writes the whole guest memory, so the ceiling
			// is generous; per-request contexts bound the fast operations.
			Timeout: 10 * time.Minute,
		},
	}
}

// put sends a configuration request. Firecracker answers 204 on success and
// carries a fault_message in the body otherwise, which is far more useful than
// the status alone.
func (c *fcClient) put(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPut, path, body)
}

func (c *fcClient) patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body)
}

func (c *fcClient) do(ctx context.Context, method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// The host is ignored by the Unix-socket transport but must be present for
	// the request to be well-formed.
	req, err := http.NewRequestWithContext(ctx, method,
		"http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var fault struct {
		Message string `json:"fault_message"`
	}
	if err := json.Unmarshal(detail, &fault); err == nil && fault.Message != "" {
		return fmt.Errorf("firecracker %s %s: %s", method, path, fault.Message)
	}
	return fmt.Errorf("firecracker %s %s: HTTP %d: %s",
		method, path, resp.StatusCode, bytes.TrimSpace(detail))
}

// The request shapes below mirror Firecracker's API. They are declared rather
// than built as maps so a typo is a compile error.

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcMachineConfig struct {
	VCPUCount  int64 `json:"vcpu_count"`
	MemSizeMiB int64 `json:"mem_size_mib"`
	// TrackDirtyPages asks KVM to log which guest pages are written, which is
	// what lets a later checkpoint capture only those pages.
	//
	// It has to be set before the VM starts and is not carried in a snapshot, so
	// a VM booted without it can never produce a diff — the decision is made at
	// create time, not at checkpoint time. The cost is KVM's own accounting for
	// every guest write.
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
	// SMT and CPU templates are left at their defaults: the platform does not
	// expose them, and enabling SMT would weaken the isolation guarantee.
}

type fcVsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type fcNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	// GuestMAC is deliberately left empty. Firecracker only advertises
	// VIRTIO_NET_F_MAC when it is set, and without that bit the guest driver
	// picks its own address.
	//
	// Pinning it would buy nothing the design needs. A snapshot carries whatever
	// link-layer address the guest ended up with, so a restore inherits it either
	// way — the constant guest IP has no MAC counterpart to keep. What a constant
	// would add is a hazard: identical link-layer addresses are only harmless
	// because each sandbox sits alone in its own namespace on a point-to-point
	// link, and the day that stops being true they collide (docs/network.md
	// section 1). Declared rather than dropped so the choice is visible here
	// rather than inferred from its absence.
	GuestMAC string `json:"guest_mac,omitempty"`
	// MTU is left unset for now: the uplink is 1500 and whether two layers of
	// NAT need it lowered has not been measured, so advertising a number nobody
	// has verified would be worse than letting the guest use its default.
	MTU int `json:"mtu,omitempty"`
}

// fcMmdsConfig binds the metadata service to the interfaces that may reach it.
//
// This is boot configuration -- it has to be set before InstanceStart, and it is
// carried inside a snapshot -- whereas the contents (fcMmds) can be written at any
// time. The split matters on the restore path, which rejects boot-specific
// configuration but still needs to hand a restored guest a fresh token.
type fcMmdsConfig struct {
	// V2 requires the guest to obtain a session token via PUT before it may read,
	// which is what stops a stray GET from an unrelated process in the guest --
	// including one following a redirect it did not choose -- from reading the
	// metadata. V1 answers a bare GET.
	Version string `json:"version"`
	// Only the sandbox's own interface. MMDS is reachable from whatever is listed
	// here, so listing more than the one interface would widen who can ask.
	NetworkInterfaces []string `json:"network_interfaces"`
}

// fcMmds is the metadata document handed to the guest.
//
// It carries the *hash* of the agent's token, never the token itself: the guest can
// read this document, so anything in it is readable by the sandbox's own root. A hash
// is enough for the agent to verify a token presented to it by noded, and useless for
// constructing one.
type fcMmds struct {
	AgentTokenHash string `json:"agentTokenHash,omitempty"`
}

type fcAction struct {
	ActionType string `json:"action_type"`
}

type fcVMState struct {
	State string `json:"state"`
}

type fcSnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type fcSnapshotLoad struct {
	SnapshotPath     string          `json:"snapshot_path"`
	MemBackend       fcMemBackend    `json:"mem_backend"`
	EnableDiffSnaps  bool            `json:"enable_diff_snapshots,omitempty"`
	ResumeVM         bool            `json:"resume_vm"`
	NetworkOverrides []fcNetOverride `json:"network_overrides,omitempty"`
}

type fcMemBackend struct {
	BackendPath string `json:"backend_path"`
	BackendType string `json:"backend_type"`
}

// fcNetOverride repoints an interface at a different tap while a snapshot is
// loading. Nothing sets it, and that is the intended state.
//
// A snapshot records the host device name its interface was attached to, and a
// restore looks for that name again. The name is beantap0 in every namespace, so
// what the snapshot recorded is already correct wherever it is restored — the
// override would only ever restate it. Sending one anyway would be worse than
// pointless: it would make the restore path look as though it depends on the
// runtime knowing the tap name, when the whole reason the tap name is a constant
// is that it does not.
//
// It is kept because it is the only escape hatch. If namespace organisation has
// to change — jailer is the likely reason (GitHub #20) — and the tap name stops
// being constant, this is the alternative to making the guest renumber itself
// after every restore, which is the failure mode docs/network.md section 1
// rejects: three more steps on the restore path, each of which fails as
// "networking works, but only sometimes".
type fcNetOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}
