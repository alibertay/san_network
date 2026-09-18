package netnode

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/tlsutil"
)

// TestTLSHandshakeBetweenNodes starts two nodes with a generated CA and node
// certificates and completes a HELLO handshake plus PING/PONG over TLS on
// loopback. The same credentials are what `sanup cert` writes for a public
// devnet.
func TestTLSHandshakeBetweenNodes(t *testing.T) {
	certDir := t.TempDir()
	paths, err := tlsutil.GenerateDevnetCerts(certDir, []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("GenerateDevnetCerts: %v", err)
	}
	ports := freePorts(t, 8)

	newTLSNode := func(offset int) *Node {
		config := testConfig()
		config.APIPort = ports[offset]
		config.PeerPort = ports[offset+1]
		config.P2PPort = ports[offset+2]
		config.ControllerPort = ports[offset+3]
		config.TLSCert = &paths.NodeCert
		config.TLSKey = &paths.NodeKey
		config.TLSCA = &paths.CACert
		config.AdvertiseHost = stringPointer("127.0.0.1")
		node, err := NewNode(config, mustIdentity(t))
		if err != nil {
			t.Fatalf("NewNode: %v", err)
		}
		if !node.Config().TLSEnabled() {
			t.Fatalf("TLSEnabled() must be true with a cert/key pair")
		}
		return node
	}

	nodeA := newTLSNode(0)
	nodeB := newTLSNode(4)

	if err := nodeA.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node A gRPC ports: %v", err)
	}
	defer nodeA.Stop()
	if err := nodeB.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node B gRPC ports: %v", err)
	}
	defer nodeB.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peer := map[string]any{
		"host":      "127.0.0.1",
		"peer_port": int64(nodeA.Config().PeerPort),
		"tls":       true,
	}
	stream, err := OpenSession(ctx, nodeB, peer, "peer_port", 10*time.Second)
	if err != nil {
		t.Fatalf("TLS OpenSession: %v", err)
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
		t.Fatalf("expected PONG over TLS, got %v", message)
	}
	if filepath.Base(paths.NodeKey) != "node.key" {
		t.Fatalf("unexpected generated key name %s", paths.NodeKey)
	}
}
