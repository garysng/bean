package s3

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DirStore is the local-directory ObjectStore for dev and CI: no S3 endpoint,
// bytes under a directory. Writes go to a temp file and rename on completion,
// so a crashed or aborted write never leaves a truncated object -- the same
// atomic guarantee BucketStore gets from multipart's deferred completion.
//
// Keys may contain slashes (blobs/<digest>, snapshots/<id>/data); each maps to
// a nested path under the root, with parent directories created on write.
type DirStore struct {
	root string
}

// NewDirStore roots a store at dir, creating it if absent.
func NewDirStore(dir string) (*DirStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("s3: create object dir: %w", err)
	}
	return &DirStore{root: dir}, nil
}

// path maps a key to a filesystem path, rejecting keys that would escape the
// root. The key is platform-generated, but it becomes a path here, so ".." and
// absolute keys are refused rather than trusted.
func (d *DirStore) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("s3: invalid object key %q", key)
	}
	clean := filepath.Clean(key)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("s3: object key escapes root: %q", key)
	}
	return filepath.Join(d.root, clean), nil
}

// Writer streams to a temp file and renames it into place on Close, so a
// crashed or aborted write never leaves a truncated object at the key.
func (d *DirStore) Writer(ctx context.Context, key string) (io.WriteCloser, error) {
	_ = ctx
	final, err := d.path(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".partial-*")
	if err != nil {
		return nil, err
	}
	return &dirWriter{f: tmp, final: final}, nil
}

// dirWriter publishes its content by rename only on a successful Close, and
// discards it on Abort.
type dirWriter struct {
	f      *os.File
	final  string
	closed bool
}

func (w *dirWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *dirWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		os.Remove(w.f.Name())
		return err
	}
	if err := w.f.Close(); err != nil {
		os.Remove(w.f.Name())
		return err
	}
	if err := os.Rename(w.f.Name(), w.final); err != nil {
		os.Remove(w.f.Name())
		return err
	}
	return nil
}

func (w *dirWriter) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	w.f.Close()
	os.Remove(w.f.Name())
}

func (d *DirStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	_ = ctx
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return f, err
}

func (d *DirStore) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	_ = ctx
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &limitedFile{f: f, r: io.LimitReader(f, length)}, nil
}

// limitedFile bounds a range read to length bytes while still closing the file.
type limitedFile struct {
	f *os.File
	r io.Reader
}

func (l *limitedFile) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedFile) Close() error               { return l.f.Close() }

func (d *DirStore) Head(ctx context.Context, key string) (int64, error) {
	_ = ctx
	p, err := d.path(key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d *DirStore) Delete(ctx context.Context, key string) error {
	_ = ctx
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
