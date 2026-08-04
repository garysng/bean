package beand

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/sbxtoken"
)

// MDKeyAgentToken carries the caller's credential.
//
// Lowercase because gRPC normalises metadata keys and a mixed-case constant would
// read back differently than it was written.
const MDKeyAgentToken = "x-bean-agent-token"

// hashSource supplies the hash a caller must match. An interface so a test can
// substitute one without a metadata service, and so the agent does not depend on how
// the value arrives.
type hashSource interface {
	AgentTokenHash(ctx context.Context) (string, error)
}

// Authenticator gates requests on the per-sandbox token.
//
// This is what replaces the isolation the agent used to get from its address family.
// On AF_VSOCK no process inside the sandbox could dial the agent at all; serving it
// over TCP -- so one address scheme covers both the agent and any port a user exposes
// -- means the sandbox's own root can now connect, and only this check distinguishes
// noded from that root.
//
// It therefore fails closed in every direction. An absent credential, an unreadable
// metadata service and an unpublished hash all deny, because each of them is a state
// in which the agent cannot tell who is calling. The one thing it must never do is
// treat "I could not determine what to require" as "require nothing".
type Authenticator struct {
	hashes hashSource
}

// NewAuthenticator reads the expected hash from the metadata service.
func NewAuthenticator() *Authenticator {
	return &Authenticator{hashes: newMMDSClient()}
}

// authorize reports whether a call may proceed.
func (a *Authenticator) authorize(ctx context.Context) error {
	expected, err := a.hashes.AgentTokenHash(ctx)
	if err != nil {
		// Unavailable and not Internal: the caller may retry, and noded's own dial
		// treats it as an agent that is not ready yet -- which is accurate, since a
		// guest whose metadata service has not answered is still starting.
		return status.Error(codes.Unavailable,
			"agent cannot determine its expected credential")
	}
	if expected == "" {
		// Nothing was published for this sandbox. Denying is the conservative
		// reading: an agent that served requests here would be an unauthenticated
		// agent reachable from inside the sandbox, and that is precisely the
		// outcome the token exists to prevent.
		return status.Error(codes.PermissionDenied, "agent has no credential configured")
	}

	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(MDKeyAgentToken)
	if len(vals) == 0 || !sbxtoken.Verify(expected, vals[0]) {
		return status.Error(codes.PermissionDenied, "invalid agent token")
	}
	return nil
}

// Unary gates unary calls.
func (a *Authenticator) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		if err := a.authorize(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream gates streaming calls.
//
// Checked once at stream establishment rather than per frame. A stream's credential
// cannot change mid-stream -- the metadata is sent with the headers -- so re-checking
// would only re-read a value that could not have been supplied differently.
func (a *Authenticator) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {
		if err := a.authorize(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// WithAgentToken attaches the credential to an outgoing call. Used by noded, which
// holds the plaintext.
func WithAgentToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, MDKeyAgentToken, token)
}
