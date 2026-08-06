package node

import (
	"context"
	"testing"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/sbxtoken"
)

// The token is minted by the manager, hashed into the runtime spec, and presented
// back on every agent call. Three separate places, and a mistake in any one of them
// produces a sandbox that works -- because the local runtime's agent has no metadata
// service and so requires no credential.
//
// That is exactly why these assert on the values rather than on a working call: a
// create that succeeds proves nothing here, and would keep proving nothing after the
// minting was deleted.

func TestCreateMintsATokenAndPublishesOnlyItsHash(t *testing.T) {
	m, _ := newNetworkedManager(t)

	sb, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx-token-1", Image: "scratch",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	m.mu.Lock()
	token := m.sandboxes["sbx-token-1"].agentToken
	m.mu.Unlock()

	if token == "" {
		t.Fatal("no agent token was minted for a networked sandbox; the agent would " +
			"have nothing to distinguish noded from a process inside the sandbox")
	}
	// What reaches the guest must be the hash. The guest can read it, so a plaintext
	// token there would be a credential the sandbox holds against itself.
	if sb.Handle == nil {
		t.Fatal("no handle")
	}
	if token == sbxtoken.Hash(token) {
		t.Fatal("Hash returned its input")
	}
	if !sbxtoken.Verify(sbxtoken.Hash(token), token) {
		t.Fatal("the minted token does not verify against its own hash")
	}
}

func TestEachSandboxGetsItsOwnToken(t *testing.T) {
	// The property that makes a disclosure survivable: reading one sandbox's
	// credential must not yield a credential for the next one.
	m, _ := newNetworkedManager(t)

	for _, id := range []string{"sbx-token-a", "sbx-token-b"} {
		if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
			SandboxId: id, Image: "scratch",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	m.mu.Lock()
	a := m.sandboxes["sbx-token-a"].agentToken
	b := m.sandboxes["sbx-token-b"].agentToken
	m.mu.Unlock()

	if a == "" || b == "" {
		t.Fatal("a sandbox was created without a token")
	}
	if a == b {
		t.Fatal("two sandboxes share one agent token; a token read out of either " +
			"would open the other, and on a real node it would open every sandbox")
	}
}

func TestNoTokenWithoutNetworking(t *testing.T) {
	// Without networking the agent sits on a Unix socket outside the guest's mount
	// namespace, unreachable from inside, so a credential would be ceremony. It also
	// matters that none is issued: the agent treats "no hash published" as a reason
	// to refuse, and that signal is only useful while it means something is wrong.
	m := newTestManager(t)

	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx-token-nonet", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	m.mu.Lock()
	token := m.sandboxes["sbx-token-nonet"].agentToken
	m.mu.Unlock()

	if token != "" {
		t.Fatalf("a token was minted for a sandbox with no networking (%q); nothing "+
			"inside the sandbox can reach that agent, and issuing one anyway makes "+
			"the unprovisioned state indistinguishable from a working one", token)
	}
}
