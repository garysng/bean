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

type fcNetOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}
