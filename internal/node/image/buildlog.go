package image

import (
	"strings"
	"sync"
)

// A build's output is both streamed to whoever is watching and summarised into
// the error if the build fails. The stream is unbounded and the summary is not,
// so the summary is accumulated as the output passes rather than by buffering
// everything: BuildKit output for a long build runs to megabytes, and a node
// holding all of it for every concurrent build to quote forty lines of it is a
// leak that only shows up under load.

// tailBuffer keeps the last n complete lines written to it, plus whatever
// trailing bytes have not yet been terminated by a newline.
type tailBuffer struct {
	mu      sync.Mutex
	n       int
	lines   []string // ring, oldest at next
	next    int
	filled  bool
	partial strings.Builder
}

func newTailBuffer(n int) *tailBuffer {
	if n < 1 {
		n = 1
	}
	return &tailBuffer{n: n, lines: make([]string, n)}
}

// Write never fails and never blocks on the reader, because it is wired
// directly to the build subprocess: a slow or absent consumer of the tail must
// not be able to stall the build.
func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rest := string(p)
	for {
		i := strings.IndexByte(rest, '\n')
		if i < 0 {
			t.partial.WriteString(rest)
			return len(p), nil
		}
		t.partial.WriteString(rest[:i])
		t.push(strings.TrimRight(t.partial.String(), "\r"))
		t.partial.Reset()
		rest = rest[i+1:]
	}
}

func (t *tailBuffer) push(line string) {
	t.lines[t.next] = line
	t.next = (t.next + 1) % t.n
	if t.next == 0 {
		t.filled = true
	}
}

// String returns the retained lines in order, including an unterminated final
// line: a build killed mid-step often leaves its most informative output there.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	if t.filled {
		out = append(out, t.lines[t.next:]...)
	}
	out = append(out, t.lines[:t.next]...)
	if p := t.partial.String(); p != "" {
		out = append(out, p)
	}
	return strings.Join(out, "\n")
}
