//go:build linux

package image

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Building images from a Dockerfile uses BuildKit rather than an in-house
// executor. COPY and ADD semantics, multi-stage builds, ARG interpolation, build
// caching, .dockerignore and heredocs add up to months of work and would still
// be an incomplete imitation; e2b and Daytona reach the same conclusion.
//
// What the platform does own is the output shape. BuildKit can export a flat
// rootfs tar, which is exactly what a base image needs — so there is no layer
// assembly, no registry round trip, and the result goes through the same writer
// as a pulled image.

// Builder produces base images from Dockerfiles.
type Builder struct {
	// Buildctl is the BuildKit client binary.
	Buildctl string
	// Addr is the buildkitd address, e.g. "unix:///run/buildkit/buildkitd.sock".
	Addr string
	// ImageDir is where finished images land, beside pulled and committed ones.
	ImageDir string
	// WorkDir holds the build context and partial images.
	WorkDir string
	// DefaultSizeMiB floors the filesystem size.
	DefaultSizeMiB int64
}

// Available reports whether this node can build, so a node advertises the
// capability only when it can honour it.
func (b *Builder) Available() error {
	if b.Buildctl == "" {
		return errors.New("image: buildctl path required")
	}
	if _, err := exec.LookPath(b.Buildctl); err != nil {
		return fmt.Errorf("image: buildctl unavailable: %w", err)
	}
	if b.Addr == "" {
		return errors.New("image: buildkitd address required")
	}
	return nil
}

// BuildRequest describes one build.
type BuildRequest struct {
	// Tag names the resulting image.
	Tag string
	// Dockerfile is the file's content. It is carried inline rather than as a
	// path in the context, so a caller can build without shipping a file.
	Dockerfile string
	// ContextTar is a tar archive of the build context, for COPY and ADD. It
	// may be empty for a Dockerfile that only runs commands.
	ContextTar []byte
	// BuildArgs are ARG values.
	BuildArgs map[string]string
	// SizeMiB bounds the resulting filesystem; zero uses the builder default.
	SizeMiB int64
}

// Build runs a Dockerfile and writes the result as a base image.
func (b *Builder) Build(ctx context.Context, req BuildRequest) (path string, err error) {
	if err := b.Available(); err != nil {
		return "", err
	}
	name, err := refToFilename(req.Tag)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Dockerfile) == "" {
		return "", errors.New("image: dockerfile required")
	}

	final := filepath.Join(b.ImageDir, name+imageSuffix)
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("image: %s already exists", req.Tag)
	}

	if err := os.MkdirAll(b.WorkDir, 0o700); err != nil {
		return "", fmt.Errorf("image: create work dir: %w", err)
	}
	// BuildKit reads the context and the Dockerfile from directories, so both
	// are laid out on disk for the duration of the build.
	buildDir, err := os.MkdirTemp(b.WorkDir, "build.*")
	if err != nil {
		return "", fmt.Errorf("image: create build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	contextDir := filepath.Join(buildDir, "context")
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		return "", fmt.Errorf("image: create context dir: %w", err)
	}
	if len(req.ContextTar) > 0 {
		if err := extractContext(req.ContextTar, contextDir); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"),
		[]byte(req.Dockerfile), 0o600); err != nil {
		return "", fmt.Errorf("image: write dockerfile: %w", err)
	}

	rootfsTar := filepath.Join(buildDir, "rootfs.tar")
	if err := b.runBuildctl(ctx, contextDir, rootfsTar, req.BuildArgs); err != nil {
		return "", err
	}

	size := req.SizeMiB
	if size <= 0 {
		size = b.sizeForTar(rootfsTar)
	}

	return writeBaseImage(b.ImageDir, b.WorkDir, req.Tag, size, func(root string) error {
		f, err := os.Open(rootfsTar)
		if err != nil {
			return fmt.Errorf("image: open build output: %w", err)
		}
		defer f.Close()
		// The same extractor the pull path uses, so whiteout handling and the
		// containment check apply to built content too.
		return extractTar(tar.NewReader(f), root)
	})
}

// runBuildctl invokes BuildKit, exporting a flat rootfs rather than an image.
func (b *Builder) runBuildctl(ctx context.Context, contextDir, outTar string,
	buildArgs map[string]string) error {

	args := []string{
		"--addr", b.Addr,
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + contextDir,
		"--local", "dockerfile=" + contextDir,
		// type=tar gives a flat filesystem, which is what a base image is.
		// Exporting an image instead would mean assembling layers only to
		// flatten them again.
		"--output", "type=tar,dest=" + outTar,
	}
	for k, v := range buildArgs {
		args = append(args, "--opt", "build-arg:"+k+"="+v)
	}

	cmd := exec.CommandContext(ctx, b.Buildctl, args...)
	// BuildKit writes its progress to stderr; on failure that output names the
	// failing step, which is the only useful thing to show a caller.
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("image: build failed: %w\n%s", err, tailLines(output.String(), 40))
	}
	return nil
}

// sizeForTar picks a filesystem size from the build output. The tar is already
// uncompressed, so its size is the content size; the headroom is for the
// filesystem's own overhead and for what the sandbox will write.
func (b *Builder) sizeForTar(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return b.floorSize()
	}
	sizeMiB := (info.Size()>>20)*2 + 128
	if floor := b.floorSize(); sizeMiB < floor {
		return floor
	}
	return sizeMiB
}

func (b *Builder) floorSize() int64 {
	if b.DefaultSizeMiB > 0 {
		return b.DefaultSizeMiB
	}
	return 512
}

// extractContext unpacks an uploaded build context.
//
// The containment check matters more here than elsewhere: this content comes
// from a client, so a crafted entry must not be able to write outside the
// context directory during extraction.
func extractContext(contextTar []byte, dest string) error {
	if err := extractTar(tar.NewReader(bytes.NewReader(contextTar)), dest); err != nil {
		return fmt.Errorf("image: extract build context: %w", err)
	}
	return nil
}

// tailLines keeps the last n lines, which is where a build's error is.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
