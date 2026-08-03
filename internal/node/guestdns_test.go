package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// newDNSTestManager builds a manager whose agents are told to configure a
// resolver. It runs the real beand binary, so it exercises the actual boot
// ordering rather than a call to WriteResolvConf in isolation -- which is the
// only way the "before the listener" claim can be tested at all.
func newDNSTestManager(t *testing.T, guestDNS string) *Manager {
	t.Helper()
	rt := runtime.NewLocalRuntime(agentBin, t.TempDir())
	rt.GuestDNS = guestDNS
	m := NewManager(rt)
	t.Cleanup(m.Close)
	return m
}

// TestGuestDNSIsWrittenBeforeAnyUserCommand is the ordering test.
//
// The first thing this does after Create returns is read the file. Create
// returns once the agent's listener answers, so if the write happened any later
// than the listener bind -- lazily on first exec, in a goroutine, after the user
// process starts -- this read is either racing it or arrives before it and sees
// nothing. There is no sleep here deliberately: a sleep would let a late write
// pass.
func TestGuestDNSIsWrittenBeforeAnyUserCommand(t *testing.T) {
	m := newDNSTestManager(t, "10.0.0.53")
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("dns-order")); err != nil {
		t.Fatal(err)
	}
	conn, rel, err := m.AgentConn(ctx, "dns-order")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	got := readGuestFile(t, ctx, conn, "/etc/resolv.conf")
	if !strings.Contains(got, "nameserver 10.0.0.53") {
		t.Fatalf("/etc/resolv.conf = %q on the first read after the agent became "+
			"reachable; a resolver written after the listener is bound is a race "+
			"that only loses under load", got)
	}
}

// TestGuestDNSOverwritesAFilesystemThatAlreadyHasOne covers the snapshot-restore
// shape: the sandbox's filesystem already carries a resolv.conf when the agent
// boots over it. The file is seeded with a different resolver so a no-op would
// be visible -- seeding it with the configured value would pass whether the
// agent wrote anything or not.
//
// Pause/Resume is deliberately not used to produce this state: on this tier they
// are SIGSTOP and SIGCONT, so the agent never re-runs its boot sequence and the
// test would assert nothing.
func TestGuestDNSOverwritesAFilesystemThatAlreadyHasOne(t *testing.T) {
	base := t.TempDir()
	rt := runtime.NewLocalRuntime(agentBin, base)
	rt.GuestDNS = "10.0.0.53"
	m := NewManager(rt)
	t.Cleanup(m.Close)
	ctx := context.Background()

	etc := filepath.Join(base, "dns-restored", "rootfs", "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "nameserver 192.168.65.7\noptions timeout:5\n"
	if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, spec("dns-restored")); err != nil {
		t.Fatal(err)
	}
	conn, rel, err := m.AgentConn(ctx, "dns-restored")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	got := readGuestFile(t, ctx, conn, "/etc/resolv.conf")
	if got != "nameserver 10.0.0.53\n" {
		t.Errorf("/etc/resolv.conf = %q after booting over an existing file; the "+
			"restored resolver must replace it rather than be appended to or "+
			"skipped", got)
	}
}

// TestLoopbackGuestDNSFailsTheSandboxRatherThanBooting is the second line of
// defence behind noded's startup check. If a loopback resolver ever reaches a
// runtime -- a caller that skipped validation, a future config path -- the
// sandbox must fail to come up. The alternative is a guest that boots, routes,
// pings a literal address and resolves nothing, which reads as a broken network
// rather than a typo in one flag.
func TestLoopbackGuestDNSFailsTheSandboxRatherThanBooting(t *testing.T) {
	m := newDNSTestManager(t, "127.0.0.53")

	if _, err := m.Create(context.Background(), spec("dns-loopback")); err == nil {
		t.Fatal("a sandbox came up with a loopback resolver; it would resolve " +
			"nothing while every layer below DNS tests clean")
	}
}

// TestNoGuestDNSLeavesImageFileAlone is the deployment that has no networking
// configured. It must behave exactly as it did before --guest-dns existed, so
// the agent must not create the file at all.
func TestNoGuestDNSLeavesImageFileAlone(t *testing.T) {
	m := newDNSTestManager(t, "")
	ctx := context.Background()

	if _, err := m.Create(ctx, spec("dns-unset")); err != nil {
		t.Fatal(err)
	}
	conn, rel, err := m.AgentConn(ctx, "dns-unset")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	c := agentv1.NewAgentServiceClient(conn)
	stream, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: "/etc/resolv.conf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Error("the agent created /etc/resolv.conf with no resolver configured; " +
			"a node without networking must be left exactly as it was")
	}
}

func readGuestFile(t *testing.T, ctx context.Context, conn *grpc.ClientConn, path string) string {
	t.Helper()
	c := agentv1.NewAgentServiceClient(conn)
	stream, err := c.ReadFile(ctx, &commonv1.ReadFileRequest{Path: path})
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	var sb strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		sb.Write(chunk.Data)
	}
	return sb.String()
}
