//go:build linux && fcintegration

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestUffdRestoreAgainstFirecracker drives a real VMM through boot, snapshot and
// a userfaultfd restore. It needs KVM and the node assets, so it is behind a
// build tag rather than skipped: a silent skip in CI would let the handshake rot.
//
//	go test -tags 'linux fcintegration' ./internal/node/runtime/ -run UffdRestore -v
//
// BEAN_TEST_ASSETS defaults to /var/lib/bean/assets and must hold firecracker,
// a guest kernel and agent.ext4. BEAN_TEST_IMAGE points at a prepared rootfs.
func TestUffdRestoreAgainstFirecracker(t *testing.T) {
	assets := envOr("BEAN_TEST_ASSETS", "/var/lib/bean/assets")
	kernel := envOr("BEAN_TEST_KERNEL", filepath.Join(assets, "vmlinux-6.1.175"))
	imagePath := os.Getenv("BEAN_TEST_IMAGE")
	if imagePath == "" {
		t.Fatal("BEAN_TEST_IMAGE must point at a prepared rootfs image")
	}
	for _, p := range []string{filepath.Join(assets, "firecracker"), kernel,
		filepath.Join(assets, "agent.ext4"), imagePath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing asset %s: %v", p, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	src := t.TempDir()
	copyFile(t, filepath.Join(assets, "agent.ext4"), filepath.Join(src, "agent.ext4"))
	copyFile(t, imagePath, filepath.Join(src, "rootfs.ext4"))

	// Boot.
	cmd, client := startProbeVMM(t, assets, src)
	putOrFail(t, ctx, client, "/machine-config",
		fcMachineConfig{VCPUCount: 1, MemSizeMiB: 512})
	putOrFail(t, ctx, client, "/boot-source", fcBootSource{
		KernelImagePath: kernel,
		BootArgs: "quiet reboot=k panic=-1 pci=off init=/bean/beand" +
			" -- --listen vsock:1024 --pivot /dev/vdb",
	})
	putOrFail(t, ctx, client, "/drives/agent", fcDrive{
		DriveID: "agent", PathOnHost: "agent.ext4",
		IsRootDevice: true, IsReadOnly: true})
	putOrFail(t, ctx, client, "/drives/rootfs", fcDrive{
		DriveID: "rootfs", PathOnHost: "rootfs.ext4"})
	putOrFail(t, ctx, client, "/vsock", fcVsock{GuestCID: 3, UDSPath: "vsock.sock"})
	putOrFail(t, ctx, client, "/actions", fcAction{ActionType: "InstanceStart"})

	// The guest has to be past early boot for the snapshot to hold anything
	// worth restoring.
	time.Sleep(3 * time.Second)

	// Pausing is a PATCH: the VM already exists, so this is a state change
	// rather than a resource being created.
	if err := client.patch(ctx, "/vm", map[string]string{"state": "Paused"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	putOrFail(t, ctx, client, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": "vmstate",
		"mem_file_path": "memory",
	})
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()

	memPath := filepath.Join(src, "memory")
	st, err := os.Stat(memPath)
	if err != nil {
		t.Fatalf("snapshot produced no memory file: %v", err)
	}
	t.Logf("memory image is %d MiB", st.Size()>>20)

	// Restore into a fresh directory, memory served on demand.
	dst := t.TempDir()
	for _, n := range []string{"vmstate", "memory", "agent.ext4", "rootfs.ext4"} {
		copyFile(t, filepath.Join(src, n), filepath.Join(dst, n))
	}
	cmd2, client2 := startProbeVMM(t, assets, dst)
	defer func() {
		_ = syscall.Kill(-cmd2.Process.Pid, syscall.SIGKILL)
		_, _ = cmd2.Process.Wait()
	}()

	h, err := newUffdHandler(filepath.Join(dst, uffdSockName), filepath.Join(dst, "memory"))
	if err != nil {
		t.Fatalf("start handler: %v", err)
	}
	defer h.Close()

	// The load blocks until the guest's first faults are served, so the handler's
	// own failure has to be reported from alongside it: waiting for the request
	// to return would just time out with no explanation.
	go func() {
		for i := 0; i < 600; i++ {
			if err := h.Err(); err != nil {
				t.Errorf("handler failed during load: %v", err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	loadCtx, loadCancel := context.WithTimeout(ctx, 20*time.Second)
	defer loadCancel()
	start := time.Now()
	err = client2.put(loadCtx, "/snapshot/load", fcSnapshotLoad{
		SnapshotPath: "vmstate",
		MemBackend:   fcMemBackend{BackendPath: uffdSockName, BackendType: "Uffd"},
		ResumeVM:     true,
	})
	elapsed := time.Since(start)
	t.Logf("faults served by the time load returned: %d", h.Faults())
	if err != nil {
		t.Fatalf("snapshot/load after %v: %v\nconsole:\n%s",
			elapsed, err, tailFile(t, filepath.Join(dst, "console.log"), 15))
	}
	t.Logf("snapshot/load returned in %v", elapsed)

	// A resumed guest touches memory immediately, so no faults at all means the
	// handshake did not connect the handler to the VM.
	deadline := time.Now().Add(10 * time.Second)
	for h.Faults() == 0 && time.Now().Before(deadline) {
		if err := h.Err(); err != nil {
			t.Fatalf("handler failed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.Faults() == 0 {
		t.Fatalf("no page faults served; console:\n%s",
			tailFile(t, filepath.Join(dst, "console.log"), 15))
	}
	t.Logf("served %d page faults", h.Faults())
	if err := h.Err(); err != nil {
		t.Errorf("handler reported: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tailFile(t *testing.T, path string, lines int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no console log: " + err.Error() + ")"
	}
	s := string(data)
	count := 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			count++
			if count > lines {
				return s[i+1:]
			}
		}
	}
	return s
}

// startProbeVMM launches a VMM whose working directory is dir, matching how the
// runtime starts one: every path Firecracker records is then relative and the
// snapshot stays portable.
func startProbeVMM(t *testing.T, assets, dir string) (*exec.Cmd, *fcClient) {
	t.Helper()
	logf, err := os.Create(filepath.Join(dir, "console.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logf.Close() })

	cmd := exec.Command(filepath.Join(assets, "firecracker"), "--api-sock", "api.sock")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(dir, "api.sock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("firecracker never created its api socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cmd, newFCClient(sock)
}

func putOrFail(t *testing.T, ctx context.Context, c *fcClient, path string, body any) {
	t.Helper()
	if err := c.put(ctx, path, body); err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
}
