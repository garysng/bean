package node

import (
	"fmt"
	"syscall"
)

// A sandbox's copy-on-write layer is sparse: it is provisioned at 20 GiB and
// costs 44 KiB. That gap is the point — it is what makes a hundred clones of one
// image cheap — but it means the scheduler's commitment ledger says nothing about
// whether the next write will find a block.
//
// What happens when it does not was measured (hack/enospc-probe.sh, decisions.md
// §3.7) and it is worse than an error:
//
//   - the failing write returns EIO and dm-snapshot marks the target Invalid
//   - subsequent writes from the guest still return success
//   - none of that data survives: the filesystem cannot be remounted afterwards
//   - the shared base image is undamaged, so the blast radius is one sandbox
//
// So there is no recovery to attempt once the disk is full — the sandbox is
// already lost, and it lost data while appearing healthy. The only useful defence
// is upstream: stop admitting new sandboxes while the node still has room for the
// ones it has. The blast radius being one sandbox is what makes that trade
// obviously right — refusing a few creates costs less than losing running work.

// DiskGuard refuses new sandboxes while free space is below a floor.
//
// It measures the filesystem rather than summing what sandboxes were promised,
// because the promise is the thing that has already been shown not to correspond
// to reality. Anything else sharing the volume — base images, the snapshot cache,
// another service — is counted automatically by asking the kernel.
type DiskGuard struct {
	// Path is any path on the filesystem to watch, normally the sandbox base
	// directory.
	Path string
	// MinFreeBytes is the floor. Zero disables the guard, which is the historical
	// behaviour: a node that has never been near full has no reason to think about
	// this, and a default in bytes would be a guess about the volume's size.
	MinFreeBytes int64
	// MinFreePercent is the floor expressed against total capacity, applied
	// alongside MinFreeBytes with whichever is larger winning. A percentage
	// travels between a 250 GB development volume and a 4 TB node where a byte
	// count does not.
	MinFreePercent float64
}

// Validate rejects a floor that cannot hold.
func (g DiskGuard) Validate() error {
	if g.MinFreeBytes < 0 {
		return fmt.Errorf("minimum free disk must not be negative, got %d", g.MinFreeBytes)
	}
	if g.MinFreePercent < 0 || g.MinFreePercent >= 100 {
		return fmt.Errorf("minimum free disk percent must be in [0,100), got %g",
			g.MinFreePercent)
	}
	return nil
}

// Enabled reports whether the guard will refuse anything.
func (g DiskGuard) Enabled() bool {
	return g.Path != "" && (g.MinFreeBytes > 0 || g.MinFreePercent > 0)
}

// DiskStats is a filesystem's real occupancy.
type DiskStats struct {
	TotalBytes int64
	FreeBytes  int64
	// UsedBytes counts blocks the filesystem has actually allocated, so a 20 GiB
	// sparse layer holding 44 KiB contributes 44 KiB.
	UsedBytes int64
}

// Stat reads the filesystem's occupancy.
//
// Free space comes from Bavail rather than Bfree: Bfree includes the reserve only
// root may consume, and admitting a sandbox into that reserve is how a node ends
// up unable to write its own logs.
func (g DiskGuard) Stat() (DiskStats, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(g.Path, &fs); err != nil {
		return DiskStats{}, fmt.Errorf("statfs %s: %w", g.Path, err)
	}
	bsize := int64(fs.Bsize)
	total := int64(fs.Blocks) * bsize
	free := int64(fs.Bavail) * bsize
	return DiskStats{
		TotalBytes: total,
		FreeBytes:  free,
		UsedBytes:  total - int64(fs.Bfree)*bsize,
	}, nil
}

// floorFor resolves the two floors into one. The larger wins so that setting
// either is a tightening rather than a possible loosening — an operator who adds
// a percentage to an existing byte count should not silently get a lower floor.
func (g DiskGuard) floorFor(total int64) int64 {
	floor := g.MinFreeBytes
	if g.MinFreePercent > 0 {
		pct := int64(float64(total) * g.MinFreePercent / 100)
		if pct > floor {
			floor = pct
		}
	}
	return floor
}

// ErrDiskPressure is returned when a node declines work to protect the sandboxes
// it already has.
type ErrDiskPressure struct {
	FreeBytes  int64
	FloorBytes int64
	Path       string
}

func (e *ErrDiskPressure) Error() string {
	return fmt.Sprintf("node is low on disk: %s has %d bytes free, below the %d-byte "+
		"floor. Admitting a sandbox here risks exhausting the copy-on-write layers "+
		"of the sandboxes already running, which is not recoverable",
		e.Path, e.FreeBytes, e.FloorBytes)
}

// Admit reports whether a new sandbox may be created.
//
// A failed measurement admits rather than refuses. The guard is a safety margin
// on top of the scheduler's accounting, and letting an unreadable statfs stop a
// node from doing any work at all would turn a monitoring problem into an outage.
func (g DiskGuard) Admit() error {
	if !g.Enabled() {
		return nil
	}
	stats, err := g.Stat()
	if err != nil {
		return nil
	}
	floor := g.floorFor(stats.TotalBytes)
	if stats.FreeBytes >= floor {
		return nil
	}
	return &ErrDiskPressure{
		FreeBytes: stats.FreeBytes, FloorBytes: floor, Path: g.Path,
	}
}
