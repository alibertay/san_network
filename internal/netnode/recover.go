package netnode

import (
	"context"
	"log"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Untrusted P2P/REST input must never take the node down. gRPC does not
// recover panics in handlers by default, so every handler runs behind these
// interceptors and the spawned session goroutine wraps PeerSession itself.

// recoverUnaryInterceptor converts a handler panic into an Internal error.
func recoverUnaryInterceptor(ctx context.Context, request any, info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (response any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("gRPC handler %s panicked: %v\n%s", info.FullMethod, recovered, debug.Stack())
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(ctx, request)
}

// recoverStreamInterceptor converts a stream handler panic into an Internal
// error.
func recoverStreamInterceptor(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo,
	handler grpc.StreamHandler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("gRPC stream %s panicked: %v\n%s", info.FullMethod, recovered, debug.Stack())
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(server, stream)
}

// runPeerSession runs the session handler, turning a panic into an error so a
// malformed message can only close its own session.
func (server *p2pServer) runPeerSession(ctx context.Context, stream *PeerStream) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("peer session panicked: %v\n%s", recovered, debug.Stack())
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return server.node.PeerSession(ctx, stream)
}
