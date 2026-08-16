package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Build logs are an append-only byte stream addressed by offset, which is
// exactly what ObjectStore serves: a build's output goes to a dedicated logs
// bucket, one immutable chunk per flush, and any reader ranges back over the
// chunks by byte offset. This is what lets the control plane hold no per-build
// log state -- every bean-api replica reads the same objects, and a restart
// loses nothing (docs/build-logs-s3.md).
//
// The writer (noded) and the reader (bean-api) share this file so the key
// scheme and the manifest shape are defined once; a mismatch between the two
// sides would silently strand a log, so neither derives its own.

// buildLogPrefix namespaces every build's objects in the logs bucket, so a
// lifecycle rule can target logs without touching anything else that shares the
// bucket.
const buildLogPrefix = "buildlogs"

// buildLogChunkDigits zero-pads a chunk's sequence so keys sort lexically in the
// order they were written. Six digits is a million chunks, far more than any
// build produces.
const buildLogChunkDigits = 6

// BuildLogKey sanitizes a build ref into a single path segment, using the same
// scheme as the image package's refToFilename: alphanumerics, '-' and '_' pass
// through; everything else (the '/', ':' and '@' a ref carries) becomes
// "_<hex>". The result is slash-free, collision-free and stable, so the writer
// and the reader derive an identical key from the same ref without sharing
// state.
func BuildLogKey(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("s3: build log ref required")
	}
	var b strings.Builder
	b.Grow(len(ref))
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "_%x", r)
		}
	}
	return b.String(), nil
}

// buildLogChunkKey names the object holding chunk seq of the build under key.
func buildLogChunkKey(key string, seq int) string {
	return fmt.Sprintf("%s/%s/%0*d", buildLogPrefix, key, buildLogChunkDigits, seq)
}

// buildLogManifestKey names the small status side-channel object for the build
// under key.
func buildLogManifestKey(key string) string {
	return fmt.Sprintf("%s/%s/manifest", buildLogPrefix, key)
}

// BuildLogManifest is the log store's own view of a build's progress, small and
// overwritten as the build advances. It lets a reader decide "is there more
// coming" from the same store it reads bytes from, without a round trip to the
// control database on every poll. The authoritative terminal status is still
// store.Template; if the two disagree, the Template wins.
type BuildLogManifest struct {
	// Chunks is how many chunk objects exist: sequences 0..Chunks-1 are present.
	Chunks int `json:"chunks"`
	// Done reports the build reached a terminal state; Failed and Reason describe
	// which, mirroring the control record so a follower can stop without it.
	Done      bool      `json:"done"`
	Failed    bool      `json:"failed,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Tunables for the chunk writer. A chunk is flushed when the buffer reaches
// BuildLogFlushBytes or BuildLogFlushInterval elapses, whichever comes first, so
// a quiet build still shows progress and a chatty one does not make an object
// per line.
const (
	BuildLogFlushBytes    = 256 << 10
	BuildLogFlushInterval = 2 * time.Second
)

// BuildLogWriter buffers a build's output and flushes it to the logs store as
// immutable, sequentially numbered chunks. It satisfies io.Writer so it drops
// straight into the builder's existing Logs sink, and its Write never fails --
// losing a log line must not fail a build. Concurrency-safe: BuildKit's copy
// goroutine calls Write while the flush timer fires from another.
type BuildLogWriter struct {
	store ObjectStore
	key   string

	mu      sync.Mutex
	buf     []byte
	seq     int // next chunk sequence to write
	closed  bool
	failed  bool
	reason  string
	lastErr error

	flushBytes    int
	flushInterval time.Duration
	stopTimer     chan struct{}
	timerDone     chan struct{}
	// now and put are seams for tests; nil uses the real clock and store.
	now func() time.Time
}

// NewBuildLogWriter starts a writer for the build ref under store. A background
// timer flushes buffered output periodically; Close stops it. The manifest is
// written on each flush and finalised by Finish.
func NewBuildLogWriter(store ObjectStore, ref string) (*BuildLogWriter, error) {
	key, err := BuildLogKey(ref)
	if err != nil {
		return nil, err
	}
	w := &BuildLogWriter{
		store:         store,
		key:           key,
		flushBytes:    BuildLogFlushBytes,
		flushInterval: BuildLogFlushInterval,
		stopTimer:     make(chan struct{}),
		timerDone:     make(chan struct{}),
		now:           time.Now,
	}
	go w.timerLoop()
	return w, nil
}

// timerLoop flushes on an interval so a quiet build still surfaces its output.
func (w *BuildLogWriter) timerLoop() {
	defer close(w.timerDone)
	t := time.NewTicker(w.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = w.flush(false)
		case <-w.stopTimer:
			return
		}
	}
}

// Write buffers p and flushes a chunk once the buffer crosses the size
// threshold. It never returns an error: the sink is wired to the build
// subprocess, and failing here would fail the build over a log write.
func (w *BuildLogWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	over := len(w.buf) >= w.flushBytes
	w.mu.Unlock()
	if over {
		_ = w.flush(false)
	}
	return len(p), nil
}

// flush writes the buffered bytes as the next chunk and updates the manifest.
// final marks the manifest Done. A flush with nothing buffered still refreshes
// the manifest when final, so a build that produced no output still ends with a
// terminal manifest.
func (w *BuildLogWriter) flush(final bool) error {
	w.mu.Lock()
	if w.closed && !final {
		w.mu.Unlock()
		return nil
	}
	chunk := w.buf
	seq := w.seq
	hasData := len(chunk) > 0
	if hasData {
		w.buf = nil
		w.seq++
	}
	w.mu.Unlock()

	ctx := context.Background()
	if hasData {
		ck := buildLogChunkKey(w.key, seq)
		if err := Put(ctx, w.store, ck, strings.NewReader(string(chunk)), int64(len(chunk))); err != nil {
			// Put back what we could not write so a later flush retries it, and
			// keep the sequence so the failed key is not skipped.
			w.mu.Lock()
			w.buf = append(chunk, w.buf...)
			w.seq = seq
			w.lastErr = err
			w.mu.Unlock()
			return err
		}
	}
	if !hasData && !final {
		// Nothing to write and not finalising: no manifest churn.
		return nil
	}
	return w.writeManifest(ctx, final)
}

// writeManifest overwrites the small status side-channel. It is last-writer-wins
// and cheap, so every chunk flush refreshes it; readers use it to know how many
// chunks exist and whether more are coming.
func (w *BuildLogWriter) writeManifest(ctx context.Context, done bool) error {
	w.mu.Lock()
	m := BuildLogManifest{
		Chunks:    w.seq,
		Done:      done,
		Failed:    w.failed,
		Reason:    w.reason,
		UpdatedAt: w.now(),
	}
	w.mu.Unlock()
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return Put(ctx, w.store, buildLogManifestKey(w.key), strings.NewReader(string(body)), int64(len(body)))
}

// Finish flushes any remaining output and writes a terminal manifest recording
// how the build ended. reason is empty on success. It is safe to call once; the
// background timer is stopped so no further chunks are written.
func (w *BuildLogWriter) Finish(failed bool, reason string) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.failed, w.reason = failed, reason
	w.mu.Unlock()

	close(w.stopTimer)
	<-w.timerDone
	return w.flush(true)
}

// BuildLogReader reads a build's log back out of the logs store by byte offset,
// holding no state the caller cannot reconstruct: it is created per request and
// any bean-api replica can create one. It is the stateless replacement for the
// in-memory ring buffer.
type BuildLogReader struct {
	store ObjectStore
	key   string

	// sizes caches chunk lengths already learned via Head, so a following reader
	// does not re-Head a chunk it has already sized. Indexed by sequence.
	sizes []int64
}

// NewBuildLogReader opens a reader for ref against store.
func NewBuildLogReader(store ObjectStore, ref string) (*BuildLogReader, error) {
	key, err := BuildLogKey(ref)
	if err != nil {
		return nil, err
	}
	return &BuildLogReader{store: store, key: key}, nil
}

// Manifest fetches the current manifest. A missing manifest is reported as
// ErrNotFound, which the caller reads as "no output yet" for a build that has
// started but not flushed.
func (r *BuildLogReader) Manifest(ctx context.Context) (BuildLogManifest, error) {
	var m BuildLogManifest
	rc, err := r.store.Get(ctx, buildLogManifestKey(r.key))
	if err != nil {
		return m, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("s3: decode build log manifest: %w", err)
	}
	return m, nil
}

// Exists reports whether any log object for the build is present, so a caller
// can distinguish "unknown build" from "build with no output yet". It checks the
// manifest and the first chunk, since a build can have flushed a chunk before
// its first manifest lands (or vice versa).
func (r *BuildLogReader) Exists(ctx context.Context) (bool, error) {
	if _, err := r.store.Head(ctx, buildLogManifestKey(r.key)); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if _, err := r.store.Head(ctx, buildLogChunkKey(r.key, 0)); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return false, nil
}

// chunkSize returns chunk seq's length, Heading it once and caching the result.
// Chunks are immutable once written, so a cached size never goes stale. A
// missing chunk is ErrNotFound, which is how ReadFrom learns it has reached the
// end of what has been flushed.
func (r *BuildLogReader) chunkSize(ctx context.Context, seq int) (int64, error) {
	if seq < len(r.sizes) {
		return r.sizes[seq], nil
	}
	n, err := r.store.Head(ctx, buildLogChunkKey(r.key, seq))
	if err != nil {
		return 0, err
	}
	for len(r.sizes) <= seq {
		r.sizes = append(r.sizes, -1)
	}
	r.sizes[seq] = n
	return n, nil
}

// ReadFrom writes the log bytes at absolute offset `off` onward to `dst`,
// returning the new offset (off + bytes written). It stops at the end of what
// has currently been flushed; a follower calls it again after the manifest
// advances. Chunks are walked in order: the sizes of chunks before the target
// are summed (from the Head cache) to locate the starting chunk, then whole
// chunks stream out via GetRange.
func (r *BuildLogReader) ReadFrom(ctx context.Context, off int64, dst io.Writer) (int64, error) {
	pos := int64(0) // absolute offset at the start of chunk seq
	for seq := 0; ; seq++ {
		size, err := r.chunkSize(ctx, seq)
		if errors.Is(err, ErrNotFound) {
			return off, nil // reached the end of flushed output
		}
		if err != nil {
			return off, err
		}
		next := pos + size
		if off >= next {
			pos = next
			continue // whole chunk is before the requested offset
		}
		// Read [max(off,pos), next) from this chunk.
		start := off - pos
		if start < 0 {
			start = 0
		}
		length := size - start
		rc, err := r.store.GetRange(ctx, buildLogChunkKey(r.key, seq), start, length)
		if err != nil {
			return off, err
		}
		n, cerr := io.Copy(dst, rc)
		rc.Close()
		off += n
		if cerr != nil {
			return off, cerr
		}
		pos = next
	}
}
