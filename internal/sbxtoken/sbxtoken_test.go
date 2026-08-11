package sbxtoken

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestVerifyAcceptsTheMintedToken(t *testing.T) {
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !Verify(Hash(tok), tok) {
		t.Fatal("a freshly minted token did not verify against its own hash")
	}
}

func TestVerifyRejectsAnotherSandboxesToken(t *testing.T) {
	// The property that makes a per-sandbox token a confinement rather than a
	// formality: holding one sandbox's token must not open another's agent.
	mine, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	theirs, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mine == theirs {
		t.Fatal("two calls to New returned the same token")
	}
	if Verify(Hash(theirs), mine) {
		t.Fatal("one sandbox's token verified against another's hash")
	}
}

func TestUnprovisionedGuestRejectsEverything(t *testing.T) {
	// An empty expected hash is what the agent reads when MMDS was never
	// populated. Accepting anything in that state would turn a provisioning
	// failure into an agent with no authentication at all, so it is checked
	// rather than left to the comparison.
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if Verify("", tok) {
		t.Fatal("an unprovisioned hash accepted a token")
	}
	if Verify("", "") {
		t.Fatal("an unprovisioned hash accepted an empty token")
	}
}

func TestMissingCredentialIsRejected(t *testing.T) {
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if Verify(Hash(tok), "") {
		t.Fatal("a request presenting no token was accepted")
	}
}

func TestTheHashDoesNotRevealTheToken(t *testing.T) {
	// What is placed in MMDS must not be usable as a credential. The guest can
	// read it, so if presenting the hash were accepted the whole arrangement
	// would collapse to a shared secret the sandbox already holds.
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := Hash(tok)
	if Verify(h, h) {
		t.Fatal("presenting the hash itself was accepted as the token")
	}
}

func TestWithAgentTokenRoundTrips(t *testing.T) {
	// The node attaches the plaintext to an outgoing call; the agent reads it
	// back off the incoming side under the same key. The two helpers have to
	// agree, so they are exercised as a pair.
	ctx := WithAgentToken(context.Background(), "tok-123")
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("WithAgentToken attached no outgoing metadata")
	}
	if got := md.Get(MDKey); len(got) != 1 || got[0] != "tok-123" {
		t.Fatalf("outgoing metadata = %v, want [tok-123]", got)
	}
}

func TestWithAgentTokenLeavesEmptyTokenOff(t *testing.T) {
	// An empty token must be absent, not present-but-empty: the agent rejects
	// both, but "no credential" is the honest state to send.
	ctx := WithAgentToken(context.Background(), "")
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if vals := md.Get(MDKey); len(vals) != 0 {
			t.Fatalf("empty token was attached as %v", vals)
		}
	}
}

func TestFromIncomingReadsTheCredential(t *testing.T) {
	md := metadata.New(map[string]string{MDKey: "tok-abc"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if got := FromIncoming(ctx); got != "tok-abc" {
		t.Fatalf("FromIncoming = %q, want tok-abc", got)
	}
}

func TestFromIncomingWithoutMetadataIsEmpty(t *testing.T) {
	if got := FromIncoming(context.Background()); got != "" {
		t.Fatalf("FromIncoming with no metadata = %q, want empty", got)
	}
}

func TestFromIncomingWithoutKeyIsEmpty(t *testing.T) {
	// Metadata present but carrying some other key: the credential is still
	// absent and must read as empty rather than panic on an empty slice.
	md := metadata.New(map[string]string{"x-other": "v"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	if got := FromIncoming(ctx); got != "" {
		t.Fatalf("FromIncoming without the key = %q, want empty", got)
	}
}

func TestFromHeaderReadsTheCredential(t *testing.T) {
	// The proxied data path carries the token as an HTTP/2 header under the
	// same key; one reader has to cover both transports.
	h := http.Header{}
	h.Set(MDKey, "tok-xyz")
	if got := FromHeader(h); got != "tok-xyz" {
		t.Fatalf("FromHeader = %q, want tok-xyz", got)
	}
	if got := FromHeader(http.Header{}); got != "" {
		t.Fatalf("FromHeader on empty header = %q, want empty", got)
	}
}
