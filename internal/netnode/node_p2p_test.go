package netnode

import (
	"context"
	"net"
	"testing"
	"time"
)

// freePorts reserves count distinct loopback ports and releases them. The
// listeners are closed before returning, so the node binds them moments later.
func freePorts(t *testing.T, count int) []int {
	t.Helper()
	ports := make([]int, 0, count)
	listeners := make([]net.Listener, 0, count)
	for i := 0; i < count; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			t.Skipf("cannot allocate a free loopback port: %v", err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	return ports
}

// TestP2PSessionHandshake starts two memory-backed nodes on ephemeral ports,
// opens a session from B to A (HELLO/HELLO_ACK) and asserts a PING/PONG
// exchange completes over the established stream.
func TestP2PSessionHandshake(t *testing.T) {
	ports := freePorts(t, 8)

	newTestNode := func(t *testing.T, portOffset int) *Node {
		t.Helper()
		config := testConfig()
		config.PeerPort = ports[portOffset]
		config.P2PPort = ports[portOffset+1]
		config.ControllerPort = ports[portOffset+2]
		config.APIPort = ports[portOffset+3]
		config.WSTimeout = 1.0
		node, err := NewNode(config, mustIdentity(t))
		if err != nil {
			t.Fatalf("NewNode: %v", err)
		}
		return node
	}

	nodeA := newTestNode(t, 0)
	nodeB := newTestNode(t, 4)

	if err := nodeA.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node A gRPC ports: %v", err)
	}
	defer nodeA.Stop()
	if err := nodeB.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node B gRPC ports: %v", err)
	}
	defer nodeB.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	peer := map[string]any{
		"host":      "127.0.0.1",
		"peer_port": int64(nodeA.Config().PeerPort),
	}
	stream, err := OpenSession(ctx, nodeB, peer, "peer_port", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer stream.Close()

	if err := stream.Send(ctx, `{"type":"PING"}`); err != nil {
		t.Fatalf("send PING: %v", err)
	}
	raw, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("receive PONG: %v", err)
	}
	message, err := decodeObject(raw)
	if err != nil {
		t.Fatalf("decode PONG: %v", err)
	}
	if messageType, _ := message["type"].(string); messageType != "PONG" {
		t.Fatalf("expected PONG, got %v", message)
	}
}
