package beand

import (
	"context"
	"errors"
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

// Serve runs the agent's HTTP handler over a listener, choosing how to speak
// HTTP/2 by transport.
//
// The node's control client dials the agent as a gRPC client, which is HTTP/2
// with prior knowledge: it sends the connection preface immediately, without an
// HTTP/1 request or upgrade. On vsock and Unix sockets that is the ONLY client,
// so those connections are handed straight to http2.Server.ServeConn with a
// background-rooted context.
//
// This is deliberately NOT h2c.NewHandler for those transports. h2c serves the
// hijacked HTTP/2 connection with ServeConnOpts.Context set to the inbound
// HTTP/1 request's context (see golang.org/x/net/http2/h2c: "Context:
// r.Context()"). net/http's connection reader cancels that request context once
// it sees the hijacked connection carrying bytes it did not expect from an
// HTTP/1 request -- which is exactly what a gRPC client's immediate frames look
// like. The cancellation cascades to every HTTP/2 stream on the connection, so
// the first RPC fails with "context canceled" before the handler's reply is
// written. A half-duplex Connect-over-HTTP/1.1 call never triggers it; a
// full-duplex gRPC call over the fast local vsock transport triggers it every
// time. Serving the connection directly with a background context removes the
// request-scoped lifetime entirely.
//
// The TCP transport still needs h2c: it carries Connect over HTTP/1.1 and
// gRPC-Web in addition to gRPC, so it cannot assume prior knowledge. The caller
// passes h2c for that case.
func Serve(lis net.Listener, h2cHandler http.Handler, directHandler http.Handler) error {
	if isPriorKnowledgeTransport(lis) {
		return serveHTTP2(lis, directHandler)
	}
	srv := &http.Server{Handler: h2cHandler}
	return srv.Serve(lis)
}

// isPriorKnowledgeTransport reports whether a listener carries only gRPC clients
// that speak HTTP/2 with prior knowledge -- vsock and Unix sockets, where the
// node is the sole caller. TCP is excluded: it also serves Connect over HTTP/1.1
// and gRPC-Web, which need the h2c negotiation path.
func isPriorKnowledgeTransport(lis net.Listener) bool {
	return isVsockListener(lis) || lis.Addr().Network() == "unix"
}

// serveHTTP2 accepts connections and serves each as cleartext HTTP/2 with prior
// knowledge, rooting every connection at a background context so no request's
// lifetime governs the connection. This is the path gRPC clients take.
func serveHTTP2(lis net.Listener, handler http.Handler) error {
	h2s := &http2.Server{}
	for {
		conn, err := lis.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return http.ErrServerClosed
			}
			return err
		}
		go h2s.ServeConn(conn, &http2.ServeConnOpts{
			Context: context.Background(),
			Handler: handler,
		})
	}
}
