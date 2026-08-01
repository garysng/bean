//go:build linux

package runtime

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Pause freezes the guest's vCPUs. Memory stays resident, so resume is
// immediate; reclaiming the memory requires a snapshot.
func (r *FCRuntime) Pause(ctx context.Context, id string) error {
	vm, err := r.get(id)
	if err != nil {
		return err
	}
	if vm.paused {
		return nil
	}
	if err := vm.client.patch(ctx, "/vm", fcVMState{State: "Paused"}); err != nil {
		return err
	}
	r.mu.Lock()
	vm.paused = true
	r.mu.Unlock()
	return nil
}

func (r *FCRuntime) Resume(ctx context.Context, id string) error {
	vm, err := r.get(id)
	if err != nil {
		return err
	}
	if !vm.paused {
		return nil
	}
	if err := vm.client.patch(ctx, "/vm", fcVMState{State: "Resumed"}); err != nil {
		return err
	}
	r.mu.Lock()
	vm.paused = false
	r.mu.Unlock()
	return nil
}

// Destroy stops the VM and releases everything it held: the VMM process, the
// rootfs device and the state directory. A leaked microVM holds memory the
// scheduler believes is available, so cleanup continues past individual
// failures rather than returning at the first one.
func (r *FCRuntime) Destroy(ctx context.Context, id string, force bool) error {
	r.mu.Lock()
	vm, ok := r.vms[id]
	if ok {
		delete(r.vms, id)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("fc: sandbox %s not found", id)
	}

	if !force {
		// SendCtrlAltDel lets the guest shut down cleanly, flushing its
		// filesystem. A guest with no ACPI handler ignores it, hence the
		// bounded wait before killing.
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := vm.client.put(shutdownCtx, "/actions", fcAction{ActionType: "SendCtrlAltDel"})
		cancel()
		if err == nil {
			select {
			case <-vm.done:
			case <-time.After(5 * time.Second):
			}
		}
	}

	r.killVMM(vm)
	var errs []error
	if err := vm.rootfs.Release(); err != nil {
		errs = append(errs, fmt.Errorf("release rootfs: %w", err))
	}
	if err := os.RemoveAll(vm.dir); err != nil {
		errs = append(errs, fmt.Errorf("remove state dir: %w", err))
	}
	return errors.Join(errs...)
}

// killVMM terminates the VMM process group and waits for it to go, so a
// following rootfs release is not fighting a process that still has the device
// open.
func (r *FCRuntime) killVMM(vm *fcVM) {
	if vm.cmd == nil || vm.cmd.Process == nil {
		return
	}
	pid := vm.cmd.Process.Pid
	// Negative pid signals the group: Firecracker is its own group leader, so
	// this reaches anything it spawned.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-vm.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-vm.done:
	case <-time.After(2 * time.Second):
	}
}

// Checkpoint writes a full Firecracker snapshot: guest memory, device state and
// the writable rootfs, bundled as a tar stream.
//
// The VM must be paused for the snapshot to be consistent, and Firecracker
// requires it. A caller that asked to keep the sandbox running gets it resumed
// afterwards; that decision belongs to the control plane, so the previous state
// is restored rather than assumed.
func (r *FCRuntime) Checkpoint(ctx context.Context, id string, w io.Writer) error {
	vm, err := r.get(id)
	if err != nil {
		return err
	}

	wasPaused := vm.paused
	if !wasPaused {
		if err := r.Pause(ctx, id); err != nil {
			return fmt.Errorf("fc: pause for snapshot: %w", err)
		}
		defer func() {
			// Resume on the way out regardless of the snapshot's outcome: a
			// failed checkpoint must not leave the sandbox frozen.
			if err := r.Resume(context.WithoutCancel(ctx), id); err != nil {
				// The sandbox is stuck paused; the caller's error, if any,
				// takes precedence, so this is reported through the console
				// log rather than swallowing the original failure.
				fmt.Fprintf(os.Stderr, "fc: resume %s after snapshot: %v\n", id, err)
			}
		}()
	}

	snapDir := filepath.Join(vm.dir, "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return fmt.Errorf("fc: create snapshot dir: %w", err)
	}
	defer os.RemoveAll(snapDir)

	statePath := filepath.Join(snapDir, snapshotStateFile)
	memPath := filepath.Join(snapDir, snapshotMemFile)
	if err := vm.client.put(ctx, "/snapshot/create", fcSnapshotCreate{
		SnapshotType: "Full", SnapshotPath: statePath, MemFilePath: memPath,
	}); err != nil {
		return err
	}

	// The rootfs travels with the snapshot: memory state referring to a
	// filesystem that has moved on since would restore into corruption.
	return writeSnapshotBundle(w, statePath, memPath, vm.rootfs.Writable)
}

// Snapshot bundle member names. Restore looks them up by name, so a bundle
// written by one version is readable by another as long as these hold.
const (
	snapshotStateFile  = "vmstate"
	snapshotMemFile    = "memory"
	snapshotRootfsFile = "rootfs"
)

// writeSnapshotBundle streams the three parts as a tar archive. Tar is used
// rather than a custom container because the parts have wildly different sizes
// and a reader must be able to find one without buffering the others.
func writeSnapshotBundle(w io.Writer, statePath, memPath, rootfsPath string) error {
	tw := tar.NewWriter(w)
	members := []struct{ name, path string }{
		{snapshotStateFile, statePath},
		{snapshotMemFile, memPath},
		{snapshotRootfsFile, rootfsPath},
	}
	for _, m := range members {
		if m.path == "" {
			continue
		}
		if err := writeTarFile(tw, m.name, m.path); err != nil {
			return fmt.Errorf("fc: bundle %s: %w", m.name, err)
		}
	}
	return tw.Close()
}

func writeTarFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// loadSnapshot unpacks a bundle and restores the VM from it. The guest resumes
// with its memory intact, which is what makes a restore cheaper than a boot.
func (r *FCRuntime) loadSnapshot(ctx context.Context, vm *fcVM, src io.Reader) error {
	restoreDir := filepath.Join(vm.dir, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		return fmt.Errorf("fc: create restore dir: %w", err)
	}

	paths, err := readSnapshotBundle(src, restoreDir, vm.rootfs.Writable)
	if err != nil {
		return err
	}
	if paths[snapshotStateFile] == "" || paths[snapshotMemFile] == "" {
		return errors.New("fc: snapshot bundle missing vmstate or memory")
	}

	// The vsock socket must be reconfigured: the restored guest expects its
	// channel, but the host path belongs to this VM, not the one snapshotted.
	if err := vm.client.put(ctx, "/vsock", fcVsock{
		GuestCID: guestCID, UDSPath: vm.vsockPath,
	}); err != nil {
		return err
	}

	return vm.client.put(ctx, "/snapshot/load", fcSnapshotLoad{
		SnapshotPath: paths[snapshotStateFile],
		MemBackend: fcMemBackend{
			BackendPath: paths[snapshotMemFile],
			BackendType: "File",
		},
		ResumeVM: true,
	})
}

// readSnapshotBundle extracts a bundle. The rootfs is written straight over the
// device the provider prepared, so the restored guest sees the filesystem it
// was snapshotted with rather than a fresh copy of the base image.
func readSnapshotBundle(src io.Reader, dir, rootfsDevice string) (map[string]string, error) {
	paths := map[string]string{}
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return paths, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fc: read snapshot bundle: %w", err)
		}

		var dest string
		switch hdr.Name {
		case snapshotStateFile, snapshotMemFile:
			dest = filepath.Join(dir, hdr.Name)
		case snapshotRootfsFile:
			if rootfsDevice == "" {
				continue
			}
			dest = rootfsDevice
		default:
			// An unknown member is skipped rather than rejected, so a bundle
			// gaining parts stays loadable by an older node.
			continue
		}

		if err := writeBundleMember(tr, dest, hdr.Name == snapshotRootfsFile); err != nil {
			return nil, fmt.Errorf("fc: extract %s: %w", hdr.Name, err)
		}
		paths[hdr.Name] = dest
	}
}

// writeBundleMember writes one member. The rootfs is written in place, since
// the device already exists at the right size; the others are created.
func writeBundleMember(src io.Reader, dest string, inPlace bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if inPlace {
		// Truncating a block device is meaningless and truncating the sparse
		// file would discard its size, so the contents are overwritten.
		flags = os.O_WRONLY
	}
	f, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	return f.Sync()
}

func (r *FCRuntime) get(id string) (*fcVM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vm, ok := r.vms[id]
	if !ok {
		return nil, fmt.Errorf("fc: sandbox %s not found", id)
	}
	return vm, nil
}
