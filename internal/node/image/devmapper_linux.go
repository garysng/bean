//go:build linux

package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DevMapperProvider gives each sandbox a copy-on-write view of a shared base
// image, using device-mapper's snapshot target.
//
// This is what makes creating a sandbox cheap. FileProvider copies the base
// image, so a 512 MiB image costs 512 MiB of disk and the time to write it, per
// sandbox. Here the base is opened read-only once and shared by every sandbox on
// the node; each one gets a sparse copy-on-write store that holds only the
// blocks it changes — measured at 80 KiB after a small write, against 512 MiB
// for a copy. Fanning out a hundred clones of an image becomes practical, which
// is the batch-evaluation case the platform exists for.
//
// The base image still has to be local. Fetching it lazily from object storage
// is a separate concern, layered under this one rather than replacing it.
type DevMapperProvider struct {
	// BaseDir holds per-sandbox copy-on-write stores.
	BaseDir string
	// ImageDir holds the shared base images.
	ImageDir string
	// DefaultSizeMiB bounds the copy-on-write store when a spec does not.
	DefaultSizeMiB int64

	mu sync.Mutex
	// bases counts sandboxes per base image, so the shared read-only loop
	// device is torn down when the last one goes rather than leaking.
	bases map[string]*sharedBase

	cache cachedRefs
}

type sharedBase struct {
	loopDev string
	sectors int64
	refs    int
}

func NewDevMapperProvider(baseDir, imageDir string, defaultSizeMiB int64) *DevMapperProvider {
	return &DevMapperProvider{
		BaseDir:        baseDir,
		ImageDir:       imageDir,
		DefaultSizeMiB: defaultSizeMiB,
		bases:          map[string]*sharedBase{},
	}
}

func (p *DevMapperProvider) Name() string { return "devmapper" }

// Available reports whether the host can run this provider, so a node fails to
// start rather than accepting placements it cannot honour.
func (p *DevMapperProvider) Available() error {
	for _, bin := range []string{"dmsetup", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("image: devmapper needs %s: %w", bin, err)
		}
	}
	out, err := exec.Command("dmsetup", "targets").Output()
	if err != nil {
		return fmt.Errorf("image: dmsetup targets: %w", err)
	}
	// The snapshot target is often a module that is not loaded until something
	// asks for it, so a missing target is worth reporting precisely.
	if !strings.Contains(string(out), "snapshot ") {
		return errors.New("image: device-mapper snapshot target unavailable (modprobe dm_snapshot)")
	}
	return nil
}

func (p *DevMapperProvider) Prepare(ctx context.Context, sandboxID, imageRef string, sizeMiB int64) (rootfs *Rootfs, err error) {
	if sandboxID == "" {
		return nil, errors.New("image: sandbox id required")
	}
	if sizeMiB <= 0 {
		sizeMiB = p.DefaultSizeMiB
	}
	if sizeMiB <= 0 {
		sizeMiB = 2048
	}

	basePath, err := p.basePath(imageRef)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(p.BaseDir, sandboxID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("image: create sandbox dir: %w", err)
	}

	// Each step registers its undo, so a failure part-way leaves no loop
	// device, no mapping and no directory. A leaked mapping would keep the base
	// image busy and eventually exhaust the loop devices.
	var cleanup []func()
	defer func() {
		if err == nil {
			return
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}()
	cleanup = append(cleanup, func() { os.RemoveAll(dir) })

	base, err := p.acquireBase(basePath)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { p.releaseBase(basePath) })

	// The copy-on-write store is sparse: it costs only the blocks written.
	cowPath := filepath.Join(dir, "cow.img")
	if err := createSparse(cowPath, sizeMiB); err != nil {
		return nil, err
	}
	cowLoop, err := attachLoop(cowPath, false)
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { detachLoop(cowLoop) })

	// "P" makes the snapshot persistent, so its exceptions survive a teardown
	// and the device can be reassembled; 8 sectors is the standard chunk size,
	// small enough that a one-block write does not copy much.
	name := dmName(sandboxID)
	table := fmt.Sprintf("0 %d snapshot %s %s P 8", base.sectors, base.loopDev, cowLoop)
	if err := run("dmsetup", "create", name, "--table", table); err != nil {
		return nil, fmt.Errorf("image: create snapshot device: %w", err)
	}
	cleanup = append(cleanup, func() { _ = run("dmsetup", "remove", "--retry", name) })

	device := filepath.Join("/dev/mapper", name)

	// Firecracker resolves drive paths relative to its working directory, which
	// is the sandbox directory, so the device is linked in under a local name.
	link := filepath.Join(dir, "rootfs.img")
	if err := os.Symlink(device, link); err != nil {
		return nil, fmt.Errorf("image: link rootfs device: %w", err)
	}

	return &Rootfs{
		Device: link,
		// The copy-on-write store is what a checkpoint has to capture: it holds
		// everything this sandbox changed, and the base is reproducible.
		Writable: cowPath,
		release: func() error {
			var errs []error
			if err := run("dmsetup", "remove", "--retry", name); err != nil {
				errs = append(errs, fmt.Errorf("remove mapping: %w", err))
			}
			if err := detachLoop(cowLoop); err != nil {
				errs = append(errs, fmt.Errorf("detach cow loop: %w", err))
			}
			p.releaseBase(basePath)
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove sandbox dir: %w", err))
			}
			return errors.Join(errs...)
		},
	}, nil
}

func (p *DevMapperProvider) Prewarm(ctx context.Context, imageRef string) error {
	_, err := p.basePath(imageRef)
	return err
}

// Cached lists the base images present locally.
func (p *DevMapperProvider) Cached() (map[string]int64, error) {
	return p.cache.get(p.ImageDir)
}

func (p *DevMapperProvider) basePath(imageRef string) (string, error) {
	name, err := refToFilename(imageRef)
	if err != nil {
		return "", err
	}
	path := filepath.Join(p.ImageDir, name+".ext4")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotCached, imageRef)
		}
		return "", err
	}
	return path, nil
}

// acquireBase returns the shared read-only loop device for a base image,
// attaching it on first use. Sharing one device across sandboxes is the point:
// it is what avoids a copy per sandbox.
func (p *DevMapperProvider) acquireBase(basePath string) (*sharedBase, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if base, ok := p.bases[basePath]; ok {
		base.refs++
		return base, nil
	}

	loopDev, err := attachLoop(basePath, true)
	if err != nil {
		return nil, err
	}
	sectors, err := deviceSectors(loopDev)
	if err != nil {
		detachLoop(loopDev)
		return nil, err
	}
	base := &sharedBase{loopDev: loopDev, sectors: sectors, refs: 1}
	p.bases[basePath] = base
	return base, nil
}

// releaseBase drops a reference and detaches the shared device when the last
// sandbox using it goes away.
func (p *DevMapperProvider) releaseBase(basePath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	base, ok := p.bases[basePath]
	if !ok {
		return
	}
	base.refs--
	if base.refs > 0 {
		return
	}
	delete(p.bases, basePath)
	detachLoop(base.loopDev)
}

// dmName derives a mapping name. Device-mapper names are a flat namespace
// shared with everything else on the host, so the prefix both avoids collisions
// and makes orphans identifiable during reconciliation.
func dmName(sandboxID string) string {
	return "bean-" + sandboxID
}

func attachLoop(path string, readOnly bool) (string, error) {
	args := []string{"--find", "--show"}
	if readOnly {
		args = append(args, "--read-only")
	}
	args = append(args, path)
	out, err := exec.Command("losetup", args...).Output()
	if err != nil {
		return "", fmt.Errorf("image: attach loop for %s: %w", path, err)
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("image: losetup returned no device for %s", path)
	}
	return dev, nil
}

func detachLoop(dev string) error {
	if dev == "" {
		return nil
	}
	return run("losetup", "-d", dev)
}

func deviceSectors(dev string) (int64, error) {
	out, err := exec.Command("blockdev", "--getsz", dev).Output()
	if err != nil {
		return 0, fmt.Errorf("image: size of %s: %w", dev, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("image: parse size of %s: %w", dev, err)
	}
	return n, nil
}

// run executes a command, folding stderr into the error: dmsetup and losetup
// explain their failures there, and the exit status alone rarely says why.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (p *DevMapperProvider) invalidateCache() { p.cache.invalidate() }
