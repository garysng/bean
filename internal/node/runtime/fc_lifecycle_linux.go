//go:build linux

package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garysng/bean/internal/obs"
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

	tracer := obs.Tracer("fc")
	// There is no graceful-shutdown step. Asking the guest to power off over
	// ACPI and waiting for it cost 5001ms of a measured 5335ms destroy and could
	// never succeed: the guest kernel is built without CONFIG_ACPI_BUTTON, so
	// nothing receives the event, and the agent is PID 1 with no signal handler.
	// Flushing is done by the manager over the agent connection before it gets
	// here, which confirms the write-out instead of waiting on a guess.
	_, kSpan := tracer.Start(ctx, "fc.killVMM")
	r.killVMM(vm)
	kSpan.End()

	var errs []error
	rCtx, rSpan := tracer.Start(ctx, "fc.releaseRootfs")
	if err := vm.rootfs.Release(); err != nil {
		errs = append(errs, fmt.Errorf("release rootfs: %w", err))
	}
	obs.Fail(rCtx, errors.Join(errs...))
	rSpan.End()

	_, dSpan := tracer.Start(ctx, "fc.removeStateDir")
	if err := os.RemoveAll(vm.dir); err != nil {
		errs = append(errs, fmt.Errorf("remove state dir: %w", err))
	}
	dSpan.End()
	return errors.Join(errs...)
}

// killVMM terminates the VMM process group and waits for it to go, so a
// following rootfs release is not fighting a process that still has the device
// open.
func (r *FCRuntime) killVMM(vm *fcVM) {
	// The page-fault handler holds a mapping of the memory image and the
	// userfault fd, neither of which the VMM's exit releases. This runs before
	// the kill so the handler is gone either way: after the VMM exits nothing
	// will fault again, and if the kill were to fail the handler would otherwise
	// stay mapped for the life of the node.
	if vm.uffd != nil {
		_ = vm.uffd.Close()
		vm.uffd = nil
	}
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
// others.
//
// Two things keep the result small. The writable layer goes in as an extent
// list, so its cost follows what the sandbox wrote rather than what it was
// provisioned. Guest memory is compressed, since most of a fresh VM's pages are
// zero. Together these took a snapshot of a small sandbox from 1280 MiB to
// around 20 MiB, which is the difference between snapshots being usable and not.
func writeSnapshotBundle(w io.Writer, statePath, memPath, rootfsPath string) error {
	// Speed over ratio: the remaining bulk is zeroed memory pages, which even
	// the fastest setting removes, and the sandbox is paused throughout.
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

	// vmstate and the memory file are dense — Firecracker writes every byte —
	// so they go in whole.
	for _, m := range []struct{ name, path string }{
		{snapshotStateFile, statePath},
		{snapshotMemFile, memPath},
	} {
		if err := writeTarFile(tw, m.name, m.path); err != nil {
			return fmt.Errorf("fc: bundle %s: %w", m.name, err)
		}
	}

	// The writable layer is provisioned large and used lightly, so it goes in as
	// an extent list. Emitting its full length as zeroes for the compressor to
	// remove measured at 15s of paused-sandbox time on a 20 GiB store.
	if rootfsPath != "" {
		if err := writeSparseTarFile(tw, snapshotRootfsFile, rootfsPath); err != nil {
			return fmt.Errorf("fc: bundle %s: %w", snapshotRootfsFile, err)
		}
	}
	return tw.Close()
}

// writeSparseTarFile writes a file's allocated extents as one tar member.
//
// A tar header needs the size up front, and the extent stream's size is only
// known after walking the file, so the extents are collected first. That costs
// memory proportional to the data written — kilobytes to a few megabytes for a
// sandbox's changes — rather than to the provisioned size.
func writeSparseTarFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if _, err := writeSparse(&buf, f, info.Size()); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: int64(buf.Len()), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, &buf)
	return err
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

// loadSnapshot restores a VM from a bundle. The guest resumes with its memory
// intact, which is what makes a restore cheaper than a boot.
//
// The bundle's reusable parts — the machine state and the memory image — are
// unpacked once per snapshot and shared by every restore of it. The writable
// rootfs is not shared: two sandboxes restored from one checkpoint diverge as
// soon as either writes, so its extents are replayed onto this sandbox's own
// device. Those extents are small (a fresh sandbox has written almost nothing),
// which is what makes the split worth having.
func (r *FCRuntime) loadSnapshot(ctx context.Context, vm *fcVM, spec *Spec, src io.Reader) error {
	entry, err := r.snapshotState(vm, spec, src)
	if err != nil {
		return err
	}

	// Guest memory is served on demand rather than read up front. With the File
	// backend Firecracker faults the whole image in through the page cache
	// before the guest runs, which costs time proportional to the guest's size
	// no matter how little of it the guest touches.
	handler, err := newUffdHandler(vm.uffdHostPath(), entry.MemPath)
	if err != nil {
		return err
	}
	vm.uffd = handler

	// Nothing may be configured before loading: Firecracker rejects a load once
	// boot-specific resources are set, because the snapshot carries the whole
	// machine configuration including the vsock device.
	//
	// The snapshotted vsock UDS path therefore has to match this VM's path,
	// which is why the path is derived from the sandbox directory the same way
	// on both sides rather than recorded in the snapshot.
	if err := vm.client.put(ctx, "/snapshot/load", fcSnapshotLoad{
		SnapshotPath: entry.StatePath,
		MemBackend: fcMemBackend{
			// Relative for the same reason the drives are: Firecracker records
			// this path and resolves it again from its own working directory.
			BackendPath: uffdSockName,
			BackendType: "Uffd",
		},
		ResumeVM: true,
	}); err != nil {
		// A load that never happened leaves a handler waiting on a connection
		// that will not come.
		handler.Close()
		vm.uffd = nil
		return err
	}
	return nil
}

// snapshotState produces the machine state and memory image to restore from,
// unpacking the bundle only if this node has not already done so for this
// snapshot. The writable rootfs is replayed onto this sandbox's device either
// way, since it cannot be shared.
func (r *FCRuntime) snapshotState(vm *fcVM, spec *Spec, src io.Reader) (snapEntry, error) {
	id := ""
	if spec != nil {
		id = spec.SnapshotID
	}

	if id != "" {
		if entry, ok := r.snapshots.Lookup(id); ok {
			// The reusable parts are already on disk, but this sandbox still
			// needs the snapshot's filesystem on its own device.
			if _, err := readSnapshotBundle(src, "", vm.rootfs.Writable); err != nil {
				return snapEntry{}, err
			}
			return entry, nil
		}
		return r.snapshots.Fill(id, src, func(dir string) (map[string]string, error) {
			return readSnapshotBundle(src, dir, vm.rootfs.Writable)
		})
	}

	// Without an id there is nothing to key a cache on, so the bundle is
	// unpacked into this sandbox's own directory.
	restoreDir := filepath.Join(vm.dir, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		return snapEntry{}, fmt.Errorf("fc: create restore dir: %w", err)
	}
	paths, err := readSnapshotBundle(src, restoreDir, vm.rootfs.Writable)
	if err != nil {
		return snapEntry{}, err
	}
	if paths[snapshotStateFile] == "" || paths[snapshotMemFile] == "" {
		return snapEntry{}, errors.New("fc: snapshot bundle missing vmstate or memory")
	}
	return snapEntry{
		StatePath: paths[snapshotStateFile],
		MemPath:   paths[snapshotMemFile],
	}, nil
}

// readSnapshotBundle extracts a bundle. The rootfs is written straight over the
// device the provider prepared, so the restored guest sees the filesystem it
// was snapshotted with rather than a fresh copy of the base image.
//
// An empty dir skips the machine state and memory image, which is what a restore
// wants when the node already holds them unpacked: the stream still has to be
// read to reach the rootfs member, but writing 512 MiB of memory image again is
// the cost the cache exists to avoid. An empty rootfsDevice likewise skips the
// filesystem.
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
			if dir == "" {
				continue
			}
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

// writeBundleMember extracts one member. sparse selects the extent-list format
// the writable layer is stored in; the dense members are copied whole.
func writeBundleMember(src io.Reader, dest string, sparse bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if sparse {
		// The destination is the layer the provider already created at the
		// right size. Truncating it would discard that, so it is written in
		// place.
		flags = os.O_WRONLY
	}
	f, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if sparse {
		return readSparse(src, f)
	}
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
