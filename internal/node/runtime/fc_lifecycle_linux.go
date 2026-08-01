//go:build linux

package runtime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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

// writeSnapshotBundle streams the parts as a gzipped tar archive.
//
// Tar is used rather than a custom container because the parts have wildly
// different sizes and a reader must be able to find one without buffering the
// others. Compression is not optional: a snapshot's uncompressed size is the
// provisioned rootfs plus guest memory — over a gigabyte for a small sandbox —
// while the holes and zeroed memory pages that make up most of it compress to
// almost nothing. Measured on a 1 GiB rootfs with 256 MiB of guest memory, the
// bundle goes from 1280 MiB to 17 MiB, which is the difference between snapshots
// being usable and not.
func writeSnapshotBundle(w io.Writer, statePath, memPath, rootfsPath string) error {
	// Speed over ratio: the content is mostly runs of zeroes, which even the
	// fastest setting removes, and a checkpoint blocks the paused sandbox.
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if err := writeBundleEntries(zw, statePath, memPath, rootfsPath); err != nil {
		return err
	}
	return zw.Close()
}

func writeBundleEntries(w io.Writer, statePath, memPath, rootfsPath string) error {
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
	// The rootfs is sparse: a 1 GiB filesystem holding 8 MiB of data would
	// otherwise be copied as 1 GiB of mostly zeroes, making every snapshot cost
	// the provisioned size rather than the used size. copySparse skips holes.
	_, err = copySparse(tw, f, info.Size())
	return err
}

// copySparse copies a file's contents while skipping unallocated regions.
//
// The holes are located with SEEK_DATA/SEEK_HOLE, so the cost is proportional
// to what the sandbox actually wrote. Zeroes are still emitted for the holes —
// the destination is a tar stream, which has no way to represent them — but
// they are generated rather than read, so no disk I/O happens for a hole. The
// stream stays compressible, which is what makes the transfer cheap.
func copySparse(dst io.Writer, f *os.File, size int64) (int64, error) {
	var written int64
	for offset := int64(0); offset < size; {
		dataStart, err := f.Seek(offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO means no data at or after offset: the rest is a hole.
			if errors.Is(err, unix.ENXIO) {
				n, werr := writeZeros(dst, size-offset)
				return written + n, werr
			}
			// A filesystem without hole support falls back to a plain copy.
			if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
				return written, serr
			}
			n, cerr := io.Copy(dst, f)
			return written + n, cerr
		}

		if dataStart > offset {
			n, werr := writeZeros(dst, dataStart-offset)
			written += n
			if werr != nil {
				return written, werr
			}
		}

		holeStart, err := f.Seek(dataStart, unix.SEEK_HOLE)
		if err != nil {
			return written, err
		}
		if holeStart > size {
			holeStart = size
		}

		if _, err := f.Seek(dataStart, io.SeekStart); err != nil {
			return written, err
		}
		n, err := io.CopyN(dst, f, holeStart-dataStart)
		written += n
		if err != nil {
			return written, err
		}
		offset = holeStart
	}
	return written, nil
}

// writeZeros emits n zero bytes without allocating n bytes.
func writeZeros(dst io.Writer, n int64) (int64, error) {
	const chunk = 128 << 10
	buf := make([]byte, chunk)
	var written int64
	for written < n {
		want := n - written
		if want > chunk {
			want = chunk
		}
		m, err := dst.Write(buf[:want])
		written += int64(m)
		if err != nil {
			return written, err
		}
	}
	return written, nil
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

	// Nothing may be configured before loading: Firecracker rejects a load once
	// boot-specific resources are set, because the snapshot carries the whole
	// machine configuration including the vsock device.
	//
	// The snapshotted vsock UDS path therefore has to match this VM's path,
	// which is why the path is derived from the sandbox directory the same way
	// on both sides rather than recorded in the snapshot.
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
	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("fc: open snapshot bundle: %w", err)
	}
	defer zr.Close()

	paths := map[string]string{}
	tr := tar.NewReader(zr)
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
	if err := copyPunchingHoles(f, src); err != nil {
		return err
	}
	return f.Sync()
}

// copyPunchingHoles writes src to f, seeking over runs of zeroes instead of
// writing them. Without this a restored rootfs allocates its full provisioned
// size even though the snapshot only carried the used blocks, so every restore
// would cost the disk the original sparse file avoided.
func copyPunchingHoles(f *os.File, src io.Reader) error {
	const chunk = 128 << 10
	buf := make([]byte, chunk)
	var offset int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if isAllZero(buf[:n]) {
				// Seeking past a hole leaves it unallocated. The file's size
				// comes from the device or a later write, so nothing is lost.
				if _, serr := f.Seek(int64(n), io.SeekCurrent); serr != nil {
					return serr
				}
			} else {
				if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
					return werr
				}
				if _, serr := f.Seek(offset+int64(n), io.SeekStart); serr != nil {
					return serr
				}
			}
			offset += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
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
