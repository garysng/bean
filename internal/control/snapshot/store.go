// Package snapshot stores checkpoint blobs. The Blobs interface is what
// lets the control plane keep snapshot bytes somewhere other than the
// control-plane disk — S3 in production (docs/snapshot-resume.md §3.1),
// a local directory in development and tests.
package snapshot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrBlobNotFound reports a missing checkpoint blob.
var ErrBlobNotFound = errors.New("snapshot blob not found")

// Blobs stores and retrieves checkpoint data by snapshot id.
type Blobs interface {
	// Writer returns a writer for a snapshot's data. Callers must Close it;
	// a failed write must not leave a readable partial blob.
	Writer(id string) (io.WriteCloser, error)
	// Reader opens a snapshot's data, returning ErrBlobNotFound if absent.
	Reader(id string) (io.ReadCloser, error)
	// Size reports stored bytes.
	Size(id string) (int64, error)
	// Delete removes a snapshot's data; deleting a missing blob is not an
	// error, so cleanup is idempotent.
	Delete(id string) error
}

// DirBlobs stores blobs as files under a directory. Writes go to a
// temporary file and are renamed on Close, so a crashed or aborted
// snapshot never leaves a truncated blob that would fail a restore in a
// confusing way.
type DirBlobs struct {
	dir string
}

func NewDirBlobs(dir string) (*DirBlobs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}
	return &DirBlobs{dir: dir}, nil
}

func (d *DirBlobs) path(id string) (string, error) {
	// Snapshot ids are platform-generated, but validate anyway: this value
	// becomes a filesystem path.
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return "", fmt.Errorf("invalid snapshot id %q", id)
	}
	return filepath.Join(d.dir, id+".tar.gz"), nil
}

func (d *DirBlobs) Writer(id string) (io.WriteCloser, error) {
	final, err := d.path(id)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(d.dir, "."+id+".partial-*")
	if err != nil {
		return nil, err
	}
	return &atomicFile{f: tmp, final: final}, nil
}

// atomicFile publishes its content only on a successful Close.
type atomicFile struct {
	f      *os.File
	final  string
	closed bool
}

func (a *atomicFile) Write(p []byte) (int, error) { return a.f.Write(p) }

func (a *atomicFile) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	if err := a.f.Sync(); err != nil {
		a.f.Close()
		os.Remove(a.f.Name())
		return err
	}
	if err := a.f.Close(); err != nil {
		os.Remove(a.f.Name())
		return err
	}
	if err := os.Rename(a.f.Name(), a.final); err != nil {
		os.Remove(a.f.Name())
		return err
	}
	return nil
}

// Abort discards a partial write without publishing it.
func (a *atomicFile) Abort() {
	if a.closed {
		return
	}
	a.closed = true
	a.f.Close()
	os.Remove(a.f.Name())
}

func (d *DirBlobs) Reader(id string) (io.ReadCloser, error) {
	p, err := d.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	return f, err
}

func (d *DirBlobs) Size(id string) (int64, error) {
	p, err := d.path(id)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d *DirBlobs) Delete(id string) error {
	p, err := d.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Aborter lets callers discard a partial write. DirBlobs writers implement
// it; an S3 implementation would abort its multipart upload.
type Aborter interface {
	Abort()
}

// AbortWrite discards a partial blob if the writer supports it, otherwise
// closing and deleting as a fallback.
func AbortWrite(blobs Blobs, id string, w io.WriteCloser) {
	if a, ok := w.(Aborter); ok {
		a.Abort()
		return
	}
	_ = w.Close()
	_ = blobs.Delete(id)
}
