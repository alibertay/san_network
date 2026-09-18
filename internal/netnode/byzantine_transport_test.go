package netnode

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netproto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// openRawSession dials a node's P2P port, completes the HELLO handshake and
// returns the raw stream plus a cleanup function.
func openRawSession(t *testing.T, ctx context.Context, address string, node *Node) (grpc.BidiStreamingClient[netproto.Envelope, netproto.Envelope], func()) {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	stream, err := netproto.NewP2PClient(connection).Session(ctx)
	if err != nil {
		connection.Close()
		t.Fatalf("Session: %v", err)
	}
	hello, err := canonical.Marshal(node.HelloPayload("HELLO"))
	if err != nil {
		connection.Close()
		t.Fatalf("marshal HELLO: %v", err)
	}
	if err := stream.Send(&netproto.Envelope{Payload: hello}); err != nil {
		connection.Close()
		t.Fatalf("send HELLO: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		connection.Close()
		t.Fatalf("receive HELLO_ACK: %v", err)
	}
	decoded, err := canonical.Decode(ack.Payload)
	ackData, _ := decoded.(map[string]any)
	if err != nil || !node.VerifyHello(ackData) {
		connection.Close()
		t.Fatalf("invalid HELLO_ACK: %v", err)
	}
	return stream, func() { _ = stream.CloseSend(); connection.Close() }
}

// TestByzantineConnectionChurnAndPartialMessages hammers a live node with
// partial messages, handshake aborts and reconnect floods, then proves the
// node still serves a clean session while the abusive identity is banned.
func TestByzantineConnectionChurnAndPartialMessages(t *testing.T) {
	ports := freePorts(t, 4)
	config := testConfig()
	config.APIPort = ports[0]
	config.PeerPort = ports[1]
	config.P2PPort = ports[2]
	config.ControllerPort = ports[3]
	config.MaxInboundPerIP = 64
	config.MaxInboundPerSubnet = 128
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Skipf("cannot bind gRPC ports: %v", err)
	}
	defer node.Stop()
	address := "127.0.0.1:" + portString(config.PeerPort)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	abuser := newDispatchNode(t)
	clean := newDispatchNode(t)

	// Handshake then partial JSON, repeated until the identity is banned; the
	// node must reject the banned identity afterwards, so stop at the ban.
	for i := 0; i < 20 && !node.peerKeyBanned(abuser.GetPublicKey()); i++ {
		stream, cleanup := openRawSession(t, ctx, address, abuser)
		if err := stream.Send(&netproto.Envelope{Payload: []byte(`{"type":"PIN`)}); err != nil {
			t.Fatalf("send partial message: %v", err)
		}
		cleanup()
	}
	// Abrupt disconnects before the handshake.
	for i := 0; i < 20; i++ {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = connection.Close()
	}

	waitFor(t, 5*time.Second, "malformed message metric", func() bool {
		return metricValue(node, "peer_malformed_messages") > 0
	})
	waitFor(t, 5*time.Second, "abusive identity ban", func() bool {
		return node.peerKeyBanned(abuser.GetPublicKey())
	})
	if metricValue(node, "peers_banned") == 0 {
		t.Fatalf("abusive peer was not counted as banned")
	}

	// The node must still answer a clean session from an unrelated identity.
	stream, cleanup := openRawSession(t, ctx, address, clean)
	defer cleanup()
	if err := stream.Send(&netproto.Envelope{Payload: []byte(`{"type":"PING"}`)}); err != nil {
		t.Fatalf("send PING: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive PONG: %v", err)
	}
	data, err := canonical.Decode(reply.Payload)
	if err != nil {
		t.Fatalf("decode PONG: %v", err)
	}
	if message, _ := data.(map[string]any)["type"].(string); message != "PONG" {
		t.Fatalf("unexpected reply: %v", data)
	}
	if node.Tip() == nil {
		t.Fatalf("node lost its chain during churn")
	}
}

// TestByzantineSessionRateLimitDropsAbusivePeer floods one session past
// SAN_PEER_RATE_LIMIT and checks the session is closed and counted.
func TestByzantineSessionRateLimitDropsAbusivePeer(t *testing.T) {
	ports := freePorts(t, 4)
	config := testConfig()
	config.APIPort = ports[0]
	config.PeerPort = ports[1]
	config.P2PPort = ports[2]
	config.ControllerPort = ports[3]
	config.PeerRateLimit = 3
	config.PeerRateWindow = 60
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Skipf("cannot bind gRPC ports: %v", err)
	}
	defer node.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, cleanup := openRawSession(t, ctx, "127.0.0.1:"+portString(config.PeerPort), node)
	defer cleanup()

	for i := 0; i < 10; i++ {
		if err := stream.Send(&netproto.Envelope{Payload: []byte(`{"type":"PING"}`)}); err != nil {
			break
		}
	}
	// The server closes the stream after the limit; drain until it does.
	recvCtx, recvCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recvCancel()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
		select {
		case <-recvCtx.Done():
			t.Fatalf("rate-limited session was not closed")
		default:
		}
	}
	if got := metricValue(node, "peer_rate_limit_hits"); got == 0 {
		t.Fatalf("rate-limit abuse was not counted")
	}
}

// TestByzantineOversizedEnvelopeDropped pins the gRPC message-size cap: an
// envelope above SAN_WS_MAX_SIZE is rejected, the stream is dropped and the
// node still serves a clean session afterwards.
func TestByzantineOversizedEnvelopeDropped(t *testing.T) {
	ports := freePorts(t, 4)
	config := testConfig()
	config.APIPort = ports[0]
	config.PeerPort = ports[1]
	config.P2PPort = ports[2]
	config.ControllerPort = ports[3]
	config.WSMaxSize = 16 * 1024
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Skipf("cannot bind gRPC ports: %v", err)
	}
	defer node.Stop()
	address := "127.0.0.1:" + portString(config.PeerPort)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	abuser := newDispatchNode(t)
	stream, cleanup := openRawSession(t, ctx, address, abuser)
	defer cleanup()

	oversized := append([]byte(`{"type":"PING","padding":"`), bytes.Repeat([]byte("x"), 64*1024)...)
	oversized = append(oversized, []byte(`"}`)...)
	if err := stream.Send(&netproto.Envelope{Payload: oversized}); err != nil {
		// The client-side send may fail immediately; that is acceptable.
		_ = err
	}
	recvCtx, recvCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recvCancel()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
		select {
		case <-recvCtx.Done():
			t.Fatalf("oversized envelope did not terminate the stream")
		default:
		}
	}

	clean := newDispatchNode(t)
	fresh, freshCleanup := openRawSession(t, ctx, address, clean)
	defer freshCleanup()
	if err := fresh.Send(&netproto.Envelope{Payload: []byte(`{"type":"PING"}`)}); err != nil {
		t.Fatalf("clean session send: %v", err)
	}
	if _, err := fresh.Recv(); err != nil {
		t.Fatalf("clean session after oversized envelope: %v", err)
	}
}

// fakeControllerServer is a minimal P2P servicer that completes the HELLO
// handshake and answers one BLOCK_VOTE_REQUEST with a test-controlled reply.
type fakeControllerServer struct {
	netproto.UnimplementedP2PServer
	node  *Node
	reply func(request map[string]any) map[string]any
}

func (server *fakeControllerServer) Session(stream grpc.BidiStreamingServer[netproto.Envelope, netproto.Envelope]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	ack, err := canonical.Marshal(server.node.HelloPayload("HELLO_ACK"))
	if err != nil {
		return err
	}
	if err := stream.Send(&netproto.Envelope{Payload: ack}); err != nil {
		return err
	}
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	decoded, err := canonical.Decode(request.Payload)
	if err != nil {
		return err
	}
	data, _ := decoded.(map[string]any)
	response, err := canonical.Marshal(server.reply(data))
	if err != nil {
		return err
	}
	return stream.Send(&netproto.Envelope{Payload: response})
}

func signVoteResponse(t *testing.T, identity *ledger.NodeIdentity, response map[string]any) map[string]any {
	t.Helper()
	payload, err := canonical.Marshal(mapWithout(response, voteMetaFields))
	if err != nil {
		t.Fatalf("marshal vote response: %v", err)
	}
	signed := deepCopyStringMap(response)
	signed["signature"] = identity.SignHex(payload)
	signed["public_key"] = identity.PublicKeyHex()
	return signed
}

// TestByzantineInvalidControllerResponsesRejected drives requestBlockVote
// against a fake controller that returns every malformed reply shape.
func TestByzantineInvalidControllerResponsesRejected(t *testing.T) {
	node := newDispatchNode(t)
	fake := newDispatchNode(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	controllerServer := &fakeControllerServer{node: fake}
	server := grpc.NewServer()
	netproto.RegisterP2PServer(server, controllerServer)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	port := listener.Addr().(*net.TCPAddr).Port
	controller := map[string]any{
		"host":            "127.0.0.1",
		"controller_port": int64(port),
		"public_key":      fake.GetPublicKey(),
		"chain_id":        node.chainID,
	}
	block := buildProposal(t, node, node.identity, nil, 0)
	base := map[string]any{
		"type":       "BLOCK_VOTE_RESPONSE",
		"chain_id":   node.chainID,
		"approved":   true,
		"block_hash": block.CurrentBlockHash,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cases := []struct {
		name  string
		reply func(map[string]any) map[string]any
		want  bool
	}{
		{"wrong type", func(map[string]any) map[string]any { return map[string]any{"type": "NOPE"} }, false},
		{"not approved", func(map[string]any) map[string]any {
			response := deepCopyStringMap(base)
			response["approved"] = false
			return signVoteResponse(t, fake.identity, response)
		}, false},
		{"wrong chain", func(map[string]any) map[string]any {
			response := deepCopyStringMap(base)
			response["chain_id"] = "other-chain"
			return signVoteResponse(t, fake.identity, response)
		}, false},
		{"wrong block hash", func(map[string]any) map[string]any {
			response := deepCopyStringMap(base)
			response["block_hash"] = strings.Repeat("ab", 32)
			return signVoteResponse(t, fake.identity, response)
		}, false},
		{"unsigned", func(map[string]any) map[string]any { return deepCopyStringMap(base) }, false},
		{"mismatched key", func(map[string]any) map[string]any {
			response := deepCopyStringMap(base)
			response["signature"] = strings.Repeat("00", 64)
			response["public_key"] = node.GetPublicKey()
			return response
		}, false},
		{"bad signature", func(map[string]any) map[string]any {
			response := deepCopyStringMap(base)
			response["signature"] = strings.Repeat("00", 64)
			response["public_key"] = fake.GetPublicKey()
			return response
		}, false},
		{"approved", func(map[string]any) map[string]any {
			return signVoteResponse(t, fake.identity, deepCopyStringMap(base))
		}, true},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			controllerServer.reply = testCase.reply
			if got := node.requestBlockVote(ctx, controller, block); got != testCase.want {
				t.Fatalf("requestBlockVote = %v, want %v", got, testCase.want)
			}
		})
	}
}

func portString(port int) string { return strconv.Itoa(port) }
