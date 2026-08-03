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
	"log/slog"
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

// Checkpoint writes a Firecracker snapshot: the writable rootfs, and — unless
// the caller opted out — guest memory and device state, bundled as a tar stream.
//
// The VM must be paused for the snapshot to be consistent, and Firecracker
// requires it. A caller that asked to keep the sandbox running gets it resumed
// afterwards; that decision belongs to the control plane, so the previous state
// is restored rather than assumed.
//
// Pausing happens for both kinds. Without memory the pause is still what makes
// the filesystem coherent: a guest writing while its device is read would put
// a torn write into the checkpoint.
func (r *FCRuntime) Checkpoint(ctx context.Context, id string, w io.Writer, opts CheckpointOptions) error {
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

	if !opts.IncludeMemory {
		// Only the filesystem. Restore boots a fresh guest from it, so nothing
		// ties the result to this host's CPU — which is the entire reason to
		// choose this over a full snapshot.
		//
		// The bundle carries just the rootfs member, and restore dispatches on
		// which members are present. That keeps the two kinds distinguishable
		// from the bundle alone, so a checkpoint stays self-describing rather
		// than depending on a database row that could disagree with it.
		return writeSnapshotBundle(w, "", "", vm.rootfs.Writable, false)
	}

	snapDir := filepath.Join(vm.dir, "snapshot")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return fmt.Errorf("fc: create snapshot dir: %w", err)
	}
	defer os.RemoveAll(snapDir)

	// A diff needs KVM to have been logging writes since the guest started, which
	// no checkpoint-time request can arrange. Refusing is the honest answer:
	// silently writing a full snapshot would hand the caller a bundle that costs
	// what it was trying to avoid, and the size alone would not explain why.
	snapType := "Full"
	if opts.Diff {
		if !vm.dirtyPages {
			return fmt.Errorf("fc: sandbox %s cannot produce a diff checkpoint: "+
				"it booted without dirty-page tracking, which cannot be enabled after the fact", id)
		}
		snapType = "Diff"
	}

	statePath := filepath.Join(snapDir, snapshotStateFile)
	memPath := filepath.Join(snapDir, snapshotMemFile)
	if err := vm.client.put(ctx, "/snapshot/create", fcSnapshotCreate{
		SnapshotType: snapType, SnapshotPath: statePath, MemFilePath: memPath,
	}); err != nil {
		return err
	}

	// The rootfs travels with the snapshot: memory state referring to a
	// filesystem that has moved on since would restore into corruption.
	return writeSnapshotBundle(w, statePath, memPath, vm.rootfs.Writable, opts.Diff)
}

// Snapshot bundle member names. Restore looks them up by name, so a bundle
// written by one version is readable by another as long as these hold.
const (
	snapshotStateFile  = "vmstate"
	snapshotMemFile    = "memory"
	snapshotRootfsFile = "rootfs"
	// snapshotMemDiffFile carries memory the guest dirtied since its base, as an
	// extent list. It is a distinct member rather than a flag beside "memory"
	// because the two must never be confused: layering a full image would erase
	// the base's untouched pages, and loading a diff on its own would give the
	// guest a memory map with holes where it expects its own state. A reader that
	// dispatches on the name cannot make either mistake, and an older node
	// encountering this member skips it and fails loudly for want of memory
	// rather than restoring something wrong.
	snapshotMemDiffFile = "memory.diff"
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
func writeSnapshotBundle(w io.Writer, statePath, memPath, rootfsPath string, diff bool) error {
	// Speed over ratio: the remaining bulk is zeroed memory pages, which even
	// the fastest setting removes, and the sandbox is paused throughout.
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if err := writeBundleEntries(zw, statePath, memPath, rootfsPath, diff); err != nil {
		return err
	}
	return zw.Close()
}

// The order of members is a performance decision, not a formatting one.
//
// Tar is sequential, so reaching a member means inflating everything ahead of it.
// Guest memory is by far the largest — 512 MiB against kilobytes for the other
// two — and it is the one member a restore can already have cached. Emitting it
// last means a cache hit, which needs only the 12 KiB writable layer, stops
// reading after a few kilobytes instead of inflating half a gigabyte to reach
// past it.
//
// Measured on a 512 MiB guest: inflating the whole stream is 489ms of a 940ms
// restore, and a cache hit was paying all of it (hack/restore-phase-probe.go).
func writeBundleEntries(w io.Writer, statePath, memPath, rootfsPath string, diff bool) error {
	tw := tar.NewWriter(w)

	// An empty state path omits the member, which is how a filesystem-only
	// checkpoint is expressed: restore sees no memory member and boots a fresh
	// guest instead of resuming one.
	if statePath != "" {
		if err := writeTarFile(tw, snapshotStateFile, statePath); err != nil {
			return fmt.Errorf("fc: bundle %s: %w", snapshotStateFile, err)
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

	if memPath != "" {
		// A full memory image is dense — Firecracker writes every byte — so it
		// goes in whole. A diff is sparse, holding only the pages the guest
		// dirtied, and has to keep that shape: which pages are real is the
		// information a later layering needs, and writing it densely would
		// present untouched holes as zeroed pages that overwrite the base.
		name, err := snapshotMemFile, error(nil)
		if diff {
			name = snapshotMemDiffFile
			err = writeSparseTarFile(tw, name, memPath)
		} else {
			err = writeTarFile(tw, name, memPath)
		}
		if err != nil {
			return fmt.Errorf("fc: bundle %s: %w", name, err)
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
func (r *FCRuntime) loadSnapshot(ctx context.Context, vm *fcVM, spec *Spec, stage *snapshotStage) error {
	entry := stage.entry

	// A checkpoint taken without memory has no machine state to load, so the
	// guest is booted onto the restored filesystem instead. Dispatching on the
	// bundle's contents rather than on a flag from the caller keeps a checkpoint
	// self-describing: the alternative is a database row that can disagree with
	// the bytes, and the disagreement would surface as a failed load.
	if entry.MemPath == "" {
		return r.configureAndBoot(ctx, vm, spec)
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
	// Dirty tracking has to be re-requested here. A snapshot does not carry the
	// setting, so without this a restored guest could never produce a diff of its
	// own — which is exactly the case that matters, since a sandbox restored from
	// a prepared checkpoint is the one most likely to be checkpointed again.
	//
	// The load also resets the dirty bitmap, so the guest's first diff covers
	// what it wrote after the restore rather than what its base wrote before.
	if err := vm.client.put(ctx, "/snapshot/load", fcSnapshotLoad{
		SnapshotPath: entry.StatePath,
		MemBackend: fcMemBackend{
			// Relative for the same reason the drives are: Firecracker records
			// this path and resolves it again from its own working directory.
			BackendPath: uffdSockName,
			BackendType: "Uffd",
		},
		EnableDiffSnaps: r.TrackDirtyPages,
		ResumeVM:        true,
	}); err != nil {
		// A load that never happened leaves a handler waiting on a connection
		// that will not come.
		handler.Close()
		vm.uffd = nil
		return err
	}
	vm.dirtyPages = r.TrackDirtyPages
	return nil
}

// snapshotState produces the machine state and memory image to restore from,
// merging the chain only if this node has not already done so for this snapshot.
// The writable layer is always extracted, to rootfsDest, because it cannot be
// shared: two sandboxes restored from one checkpoint diverge as soon as either
// writes.
func (r *FCRuntime) snapshotState(rootfsDest string, spec *Spec, layers []SnapshotLayer) (snapEntry, error) {
	id := ""
	if spec != nil {
		id = spec.SnapshotID
	}

	if id != "" {
		if entry, ok := r.snapshots.Lookup(id); ok {
			// Recorded before the layers are drained, so eviction can tell a leaf a
			// fan-out is hammering from one restored months ago. Failure is ignored:
			// a stale timestamp costs a re-unpack, while failing the restore over it
			// would trade a cheap loss for an expensive one.
			_ = r.snapshots.Touch(id)
			// The merged image is already on disk, so the layers are read only for
			// the leaf's filesystem. This is what makes a fan-out cheap: a chain is
			// merged once per node and every later restore of the same leaf skips
			// it, which matters more the longer the chain is.
			//
			// Every layer is still drained. The sender streams the whole chain
			// without knowing what this node has cached, so an unread layer would
			// leave it blocked.
			for i, layer := range layers {
				dest := ""
				if i == len(layers)-1 {
					dest = rootfsDest
				}
				if _, err := readSnapshotBundle(layer.Data, "", dest); err != nil {
					return snapEntry{}, fmt.Errorf("fc: read layer %s: %w", layer.ID, err)
				}
			}
			return entry, nil
		}
		entry, err := r.snapshots.Fill(id, func(dir string) (snapEntry, error) {
			return mergeChain(layers, dir, rootfsDest)
		})
		if err != nil {
			return snapEntry{}, err
		}
		// Swept after the entry is published rather than before, so a restore never
		// waits on eviction to reach its own image. The entry just added is pinned
		// by the caller, so this cannot reclaim what it just built.
		r.sweepSnapshotCache()
		return entry, nil
	}

	// Without an id there is nothing to key a cache on, so the merged image is
	// written beside the writable layer and discarded with it.
	return mergeChain(layers, filepath.Dir(rootfsDest), rootfsDest)
}

// sweepSnapshotCache reclaims cold cache entries if the cache is over its
// watermark. Failure is logged rather than returned: a restore that produced a
// valid image has succeeded, and refusing it because the disk could not be tidied
// afterwards would turn a housekeeping problem into an outage. The node running
// short of disk is the separate, visible signal.
func (r *FCRuntime) sweepSnapshotCache() {
	freed, err := r.snapshots.Evict(r.SnapshotCache)
	if err != nil {
		slog.Warn("snapshot cache sweep failed", "err", err)
		return
	}
	if freed > 0 {
		slog.Info("snapshot cache swept", "freedBytes", freed,
			"highBytes", r.SnapshotCache.HighBytes, "lowBytes", r.SnapshotCache.LowBytes)
	}
}

// readSnapshotBundle extracts a bundle to files, writing the writable layer's
// extent stream to rootfsDest. It does not touch any block device: the layer is
// applied to one later, while the provider is assembling it.
//
// An empty dir skips the machine state and memory image, which is what a restore
// wants when the node already holds them unpacked. An empty rootfsDest likewise
// skips the filesystem.
//
// Skipping a member means not decompressing it either. Guest memory is emitted
// last precisely so a restore that already holds it can stop inflating once it has
// the writable layer, which measured at 489ms of a 940ms restore — paid on every
// cache hit, for nothing.
//
// Stopping the inflation is not the same as stopping the read. The sender streams
// the whole bundle without knowing what this node has cached, so the remaining
// bytes still have to be consumed or the sender blocks on a stream nobody is
// reading and the restore fails with EOF. They are drained compressed: reading
// 16 MiB off the wire costs almost nothing next to inflating the 512 MiB inside.
func readSnapshotBundle(src io.Reader, dir, rootfsDest string) (map[string]string, error) {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("fc: open snapshot bundle: %w", err)
	}
	defer zr.Close()

	// What this caller is here for. A restore with a cache hit wants only the
	// writable layer; one without wants the machine state and memory too.
	wantState := dir != ""
	wantRootfs := rootfsDest != ""

	// Whatever happens, the stream is consumed to its end. Deferred rather than
	// written at each exit because there are several, and one that forgot would
	// hang a sender rather than fail visibly.
	defer func() { _, _ = io.Copy(io.Discard, src) }()

	paths := map[string]string{}
	tr := tar.NewReader(zr)
	for {
		// Everything asked for has been extracted, so there is no reason to inflate
		// what remains. A filesystem-only checkpoint has no memory member at all,
		// which is why this is checked before reading rather than after.
		if !wantState && !wantRootfs {
			return paths, nil
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return paths, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fc: read snapshot bundle: %w", err)
		}

		var dest string
		switch hdr.Name {
		case snapshotStateFile, snapshotMemFile, snapshotMemDiffFile:
			if dir == "" {
				continue
			}
			dest = filepath.Join(dir, hdr.Name)
			// Memory is the last thing written and the largest, so seeing it means
			// the machine state came earlier and nothing further is wanted from the
			// stream. A checkpoint carries one memory member or none.
			if hdr.Name != snapshotStateFile {
				wantState = false
			}
		case snapshotRootfsFile:
			if rootfsDest == "" {
				continue
			}
			dest = rootfsDest
			wantRootfs = false
		default:
			// An unknown member is skipped rather than rejected, so a bundle
			// gaining parts stays loadable by an older node.
			continue
		}

		if err := writeBundleMember(tr, dest); err != nil {
			return nil, fmt.Errorf("fc: extract %s: %w", hdr.Name, err)
		}
		paths[hdr.Name] = dest
	}
}

// writeBundleMember copies one member to a staging file, verbatim.
//
// The writable layer stays in its extent-list form here rather than being
// expanded: it is decoded once, onto the device, while the provider assembles
// it. Expanding it now and copying the result later would decode it twice, and
// writing it onto a device at this point is what corrupted a restore — a
// device-mapper snapshot has already read its exception table by then, so the
// bytes land in a store the kernel is no longer consulting.
func writeBundleMember(src io.Reader, dest string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
