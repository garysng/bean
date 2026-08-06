package beand

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/sbxtoken"
)

type fakeHashes struct {
	hash string
	err  error
}

func (f fakeHashes) AgentTokenHash(context.Context) (string, error) {
	return f.hash, f.err
}

// withToken builds the context noded produces, by the same route: through the
// exported helper, so a change to the metadata key cannot make these tests pass
// while noded and the agent disagree about it.
func withToken(t *testing.T, token string) context.Context {
	t.Helper()
	outgoing := sbxtoken.WithAgentToken(context.Background(), token)
	md, ok := metadata.FromOutgoingContext(outgoing)
	if !ok {
		t.Fatal("WithAgentToken attached no metadata")
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestTheHolderOfTheTokenIsAdmitted(t *testing.T) {
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := &Authenticator{hashes: fakeHashes{hash: sbxtoken.Hash(tok)}}
	if err := a.authorize(withToken(t, tok)); err != nil {
		t.Fatalf("the correct token was refused: %v", err)
	}
}

func TestSandboxRootWithoutTheTokenIsRefused(t *testing.T) {
	// The case that matters now that the agent is reachable over TCP: a process
	// inside the sandbox can connect, and this is the only thing between it and
	// the agent's API.
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := &Authenticator{hashes: fakeHashes{hash: sbxtoken.Hash(tok)}}

	if err := a.authorize(context.Background()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a call with no metadata got %v, want PermissionDenied", err)
	}
	if err := a.authorize(withToken(t, "guessed")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a wrong token got %v, want PermissionDenied", err)
	}
}

func TestPresentingTheHashIsRefused(t *testing.T) {
	// The hash is published through the metadata service, which the sandbox can
	// read. If presenting it were accepted, the credential would be one the
	// sandbox already holds.
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := sbxtoken.Hash(tok)
	a := &Authenticator{hashes: fakeHashes{hash: h}}
	if err := a.authorize(withToken(t, h)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("presenting the published hash got %v, want PermissionDenied", err)
	}
}

func TestAnotherSandboxesTokenIsRefused(t *testing.T) {
	mine, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	theirs, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := &Authenticator{hashes: fakeHashes{hash: sbxtoken.Hash(theirs)}}
	if err := a.authorize(withToken(t, mine)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another sandbox's token got %v, want PermissionDenied", err)
	}
}

func TestAnUnreachableMetadataServiceDeniesRatherThanAdmits(t *testing.T) {
	// The failure mode this whole arrangement turns on. If a metadata service that
	// cannot be read were treated as "no credential required", then breaking the
	// metadata service would be a way to reach an unauthenticated agent -- and
	// breaking it is something a sandbox with iptables can attempt.
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := &Authenticator{hashes: fakeHashes{err: errors.New("no route to host")}}
	err = a.authorize(withToken(t, tok))
	if err == nil {
		t.Fatal("an unreadable metadata service admitted the call")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("got %v, want Unavailable so the caller retries", code)
	}
}

func TestAnUnpublishedHashDeniesRatherThanAdmits(t *testing.T) {
	// A reachable metadata service with nothing published. Distinct from the error
	// above: here the read succeeded and the answer was "nothing", which is a
	// provisioning failure rather than a transport one, and must not open the agent.
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := &Authenticator{hashes: fakeHashes{hash: ""}}
	if err := a.authorize(withToken(t, tok)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unpublished hash got %v, want PermissionDenied", err)
	}
	if err := a.authorize(context.Background()); err == nil {
		t.Fatal("an unpublished hash admitted a call with no credential at all")
	}
}
