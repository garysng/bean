package node

import (
	"sync"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

// Build output goes out as it arrives rather than being buffered and sent at the
// end. The whole point of streaming it is to see which layer a build is on while
// it is still on it, and a buffer defeats that no matter how it is flushed.
//
// Nothing here retains the log. The node is not the right place to keep it: it
// would have to be evicted on some policy the node cannot know, and the caller
// that asked for the build is the one that knows whether anybody wants it. The
// control plane keeps it (see internal/control/api/buildlog.go).

// buildLogSender adapts an io.Writer to the build stream.
type buildLogSender struct {
	stream nodev1.SandboxService_BuildImageServer

	mu     sync.Mutex
	closed bool
	// sendErr latches the first send failure. Writing keeps succeeding after
	// one: the writer is wired to the build subprocess, and returning an error
	// there would fail the build because its logs could not be delivered.
	sendErr error
}

func newBuildLogSender(stream nodev1.SandboxService_BuildImageServer) *buildLogSender {
	return &buildLogSender{stream: stream}
}

// Write sends p as one log frame.
//
// The lock is not for concurrent writers -- os/exec drives this from a single
// copy goroutine -- but for the ordering against close, which runs on the
// handler's goroutine once the build returns.
func (s *buildLogSender) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.sendErr != nil {
		return len(p), nil
	}
	// Copied because gRPC serialises the frame after Send returns in some
	// configurations, while os/exec reuses its copy buffer immediately.
	data := make([]byte, len(p))
	copy(data, p)
	// Sent here, not accumulated for close to flush. Holding it would make this
	// exactly the buffered implementation described above: every "the logs came
	// through" assertion would still pass, and nobody would learn which layer a
	// running build is on -- which is the only reason to stream.
	if err := s.stream.Send(&nodev1.BuildImageEvent{
		Event: &nodev1.BuildImageEvent_Log{Log: data},
	}); err != nil {
		s.sendErr = err
	}
	return len(p), nil
}

// close stops further frames. Frames go out from Write, so this flushes
// nothing; what it does is guarantee no log frame can be sent afterwards.
//
// That is what makes the handler's result frame safe to send. A gRPC stream does
// not allow concurrent Send, and the writer is driven by whatever goroutine the
// builder copies output from, so the result frame needs the log side provably
// quiet rather than merely finished. Idempotent so the handler can call it
// before the result and still defer it for the failure paths.
func (s *buildLogSender) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
}
