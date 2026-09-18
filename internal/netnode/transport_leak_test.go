package netnode

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/netproto"

	"google.golang.org/grpc"
)

// stallingServer completes the HELLO handshake and then stops reading, so a
// flooding client fills the gRPC flow-control window and its sender goroutine
// blocks inside stream.Send.
type stallingServer struct {
	netproto.UnimplementedP2PServer
	hello []byte
}

func (server *stallingServer) Session(stream grpc.BidiStreamingServer[netproto.Envelope, netproto.Envelope]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&netproto.Envelope{Payload: server.hello}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

// TestOutboundSenderDoesNotLeakOnFlowControl is the F15 regression: with a
// peer that stops reading, PeerStream.Close must still return (and unblock the
// sender) instead of waiting forever for a goroutine stuck in gRPC flow
// control.
func TestOutboundSenderDoesNotLeakOnFlowControl(t *testing.T) {
	identity := mustIdentity(t)
	helloNode, err := NewNode(testConfig(), identity)
	if err != nil {
		t.Fatalf("NewNode(hello): %v", err)
	}
	ack, err := encodeObject(helloNode.HelloPayload("HELLO_ACK"))
	if err != nil {
		t.Fatalf("HelloPayload: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback listener: %v", err)
	}
	grpcServer := grpc.NewServer()
	netproto.RegisterP2PServer(grpcServer, &stallingServer{hello: ack})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	node, err := NewNode(testConfig(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode(client): %v", err)
	}

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer := map[string]any{
		"host":      "127.0.0.1",
		"peer_port": int64(listener.Addr().(*net.TCPAddr).Port),
	}
	stream, err := OpenSession(ctx, node, peer, "peer_port", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	// Flood the stream without ever reading. The server stops reading after
	// the handshake, so the client sender eventually blocks on flow control.
	payload := make([]byte, 16*1024)
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for i := 0; i < 512; i++ {
			if err := stream.Send(ctx, string(payload)); err != nil {
				return
			}
		}
	}()
	time.Sleep(750 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		stream.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("PeerStream.Close blocked: the blocked sender goroutine leaked")
	}
	select {
	case <-floodDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("flood sender did not unblock after Close")
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseline+2 {
		t.Fatalf("goroutines did not unwind: baseline=%d after=%d", baseline, after)
	}
}
