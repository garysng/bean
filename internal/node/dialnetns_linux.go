//go:build linux

package node

import (
	"context"
	"fmt"
	"net"
	goruntime "runtime"

	"golang.org/x/sys/unix"
)

// dialInNetns connects to addr from inside the network namespace at nsPath.
//
// A sandbox's agent listens on an address that only exists in that sandbox's
// namespace, and every sandbox uses the same one -- 172.31.0.2 by design, so that a
// snapshot restores with the address it was taken with. The address therefore does
// not identify the sandbox; the namespace does. Dialling from the host namespace
// would either fail or, worse, reach a different sandbox.
//
// Only the connect happens inside. Once established, the socket is an ordinary file
// descriptor with no namespace affinity, so reads and writes afterwards need nothing
// special. That is what keeps this to a single narrow window rather than a
// requirement on every use of the connection.
//
// setns is per-thread, and the Go runtime moves goroutines between threads at every
// blocking point. So the thread is locked for the whole sequence, the work happens on
// a dedicated goroutine, and nothing between the two setns calls may block on
// anything that could reschedule it. This is the same constraint startInNetns
// documents for spawning the VMM, and the reason it cost a day there.
func dialInNetns(ctx context.Context, nsPath, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		goruntime.LockOSThread()

		host, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			goruntime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("agent dial: open host netns: %w", err)}
			return
		}
		defer unix.Close(host)

		target, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			goruntime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("agent dial: open %s: %w", nsPath, err)}
			return
		}
		defer unix.Close(target)

		if err := unix.Setns(target, unix.CLONE_NEWNET); err != nil {
			goruntime.UnlockOSThread()
			ch <- result{nil, fmt.Errorf("agent dial: enter %s: %w", nsPath, err)}
			return
		}

		var d net.Dialer
		conn, dialErr := d.DialContext(ctx, "tcp", addr)

		// Restore before unlocking, and treat a failure as fatal to this thread: a
		// thread left in the sandbox's namespace would be handed back to the runtime
		// and used for unrelated work, which would then silently run inside a
		// sandbox's network. Not unlocking makes the runtime destroy the thread
		// instead, which costs a thread and cannot corrupt anything.
		if err := unix.Setns(host, unix.CLONE_NEWNET); err != nil {
			if conn != nil {
				conn.Close()
			}
			ch <- result{nil, fmt.Errorf("agent dial: restore host netns: %w", err)}
			return
		}
		goruntime.UnlockOSThread()
		ch <- result{conn, dialErr}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.conn, nil
	case <-ctx.Done():
		// The goroutine is not abandoned: DialContext observes the same context, so
		// it returns and the buffered channel accepts the result with nobody reading.
		return nil, ctx.Err()
	}
}
