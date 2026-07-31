package agent

import (
	"bytes"
	"sync"
)

// cappedBuffer keeps at most max bytes; further writes are dropped and flagged.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func newCappedBuffer(max int64) *cappedBuffer {
	return &cappedBuffer{max: max}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remain := c.max - int64(c.buf.Len())
	if remain <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remain {
		c.buf.Write(p[:remain])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out
}

func (c *cappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// RingBuffer is a fixed-capacity byte ring for user-process logs.
type RingBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	w    int
	full bool
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{buf: make([]byte, size), size: size}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		copy(r.buf, p[n-r.size:])
		r.w = 0
		r.full = true
		return n, nil
	}
	end := r.w + n
	if end <= r.size {
		copy(r.buf[r.w:], p)
	} else {
		k := r.size - r.w
		copy(r.buf[r.w:], p[:k])
		copy(r.buf, p[k:])
		r.full = true
	}
	r.w = end % r.size
	if end >= r.size {
		r.full = true
	}
	return n, nil
}

// Snapshot returns the buffered bytes in chronological order.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.w)
		copy(out, r.buf[:r.w])
		return out
	}
	out := make([]byte, r.size)
	copy(out, r.buf[r.w:])
	copy(out[r.size-r.w:], r.buf[:r.w])
	return out
}
