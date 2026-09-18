package netnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/netproto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// inboundRemoteAddress returns the observed remote address of a gRPC call, or
// "" when the transport does not expose it.
func inboundRemoteAddress(ctx context.Context) string {
	remote, ok := peer.FromContext(ctx)
	if !ok || remote.Addr == nil {
		return ""
	}
	return remote.Addr.String()
}

// Per-session buffers are bounded so a flooding peer applies backpressure
// instead of growing memory; the semaphore caps concurrent sessions.
const (
	SessionQueueSize      = 256
	MaxConcurrentSessions = 256
	MaxConcurrentSyncs    = 8
)

// PeerClosed is raised when the other side closed the session.
type PeerClosed struct{ Message string }

func (e *PeerClosed) Error() string { return e.Message }

// IsPeerClosed reports whether err is a PeerClosed error.
func IsPeerClosed(err error) bool {
	var target *PeerClosed
	return errors.As(err, &target)
}

// NodeTransport is the slice of the node the transport needs.
type NodeTransport interface {
	PeerSession(ctx context.Context, stream *PeerStream) error
	GetSyncPayload(fromIndex int64, limit int) map[string]any
	Peers() []any
	PeerStatus() map[string]any
	WSMaxSize() int
	TransportServerCredentials() credentials.TransportCredentials
	TransportClientCredentials(peer map[string]any) credentials.TransportCredentials
	HelloPayload(messageType string) map[string]any
	VerifyHello(data map[string]any) bool
	SelfPeerRecord() map[string]any
	NoteInboundPeer(address string)
	AdmitInboundPeer(address string) bool
	ReleaseInboundPeer(address string)
}

// PeerStream is a JSON-message session over a gRPC bidirectional stream.
type PeerStream struct {
	inbound       chan string
	outbound      chan string
	done          chan struct{}
	closeOnce     sync.Once
	closed        atomic.Bool
	closeCallback func()
	Label         string
	// PeerKey is the authenticated public key learned from the handshake
	// ("" before the handshake completes).
	PeerKey string
}

func newPeerStream(inbound, outbound chan string, label string) *PeerStream {
	return &PeerStream{
		inbound:  inbound,
		outbound: outbound,
		done:     make(chan struct{}),
		Label:    label,
	}
}

// Done is closed when the stream is torn down.
func (stream *PeerStream) Done() <-chan struct{} { return stream.done }

// Recv returns the next inbound message.
func (stream *PeerStream) Recv(ctx context.Context) (string, error) {
	if stream.closed.Load() {
		return "", &PeerClosed{Message: stream.Label + ": session closed"}
	}
	select {
	case message, ok := <-stream.inbound:
		if !ok {
			return "", &PeerClosed{Message: stream.Label + ": session closed"}
		}
		return message, nil
	case <-stream.done:
		return "", &PeerClosed{Message: stream.Label + ": session closed"}
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Send queues a message for the outbound stream.
func (stream *PeerStream) Send(ctx context.Context, data string) error {
	if stream.closed.Load() {
		return &PeerClosed{Message: stream.Label + ": session closed"}
	}
	select {
	case stream.outbound <- data:
		return nil
	case <-stream.done:
		return &PeerClosed{Message: stream.Label + ": session closed"}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close tears the session down once.
func (stream *PeerStream) Close() {
	stream.closeOnce.Do(func() {
		stream.closed.Store(true)
		close(stream.done)
		if stream.closeCallback != nil {
			stream.closeCallback()
		}
	})
}

// ---------------------------------------------------------------------- #
// Server side
// ---------------------------------------------------------------------- #

type p2pServer struct {
	netproto.UnimplementedP2PServer
	node     NodeTransport
	sessions chan struct{}
	syncs    chan struct{}
}

// NewP2PServer builds the gRPC servicer for a node.
func NewP2PServer(node NodeTransport) netproto.P2PServer {
	return &p2pServer{
		node:     node,
		sessions: make(chan struct{}, MaxConcurrentSessions),
		syncs:    make(chan struct{}, MaxConcurrentSyncs),
	}
}

func (server *p2pServer) acquire(ctx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (server *p2pServer) release(slots chan struct{}) { <-slots }

// Session runs the bidirectional consensus stream.
func (server *p2pServer) Session(stream grpc.BidiStreamingServer[netproto.Envelope, netproto.Envelope]) error {
	ctx := stream.Context()
	if !server.acquire(ctx, server.sessions) {
		return status.Error(codes.ResourceExhausted, "too many sessions")
	}
	defer server.release(server.sessions)

	if remote := inboundRemoteAddress(ctx); remote != "" {
		server.node.NoteInboundPeer(remote)
		if !server.node.AdmitInboundPeer(remote) {
			return status.Error(codes.ResourceExhausted, "inbound peer cap reached")
		}
		defer server.node.ReleaseInboundPeer(remote)
	}

	label := "grpc-session"
	inbound := make(chan string, SessionQueueSize)
	outbound := make(chan string, SessionQueueSize)
	peerStream := newPeerStream(inbound, outbound, label)

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(inbound)
		for {
			envelope, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case inbound <- string(envelope.Payload):
			case <-peerStream.Done():
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	peerStream.closeCallback = func() {
		// Nothing extra: the reader exits via done, the sender drains.
	}

	sessionErr := make(chan error, 1)
	go func() { sessionErr <- server.runPeerSession(ctx, peerStream) }()

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case message := <-outbound:
				if err := stream.Send(&netproto.Envelope{Payload: []byte(message)}); err != nil {
					sendErr <- err
					return
				}
			case <-peerStream.Done():
				// Flush messages queued before the close.
				for {
					select {
					case message := <-outbound:
						if err := stream.Send(&netproto.Envelope{Payload: []byte(message)}); err != nil {
							sendErr <- err
							return
						}
					default:
						sendErr <- nil
						return
					}
				}
			case <-ctx.Done():
				sendErr <- ctx.Err()
				return
			}
		}
	}()

	select {
	case <-sessionErr:
	case <-readerDone:
	case <-sendErr:
	case <-ctx.Done():
	}
	peerStream.Close()
	select {
	case <-sendErr:
	case <-time.After(time.Second):
	}
	select {
	case <-readerDone:
	case <-time.After(time.Second):
	}
	return nil
}

// Sync serves one page of blocks.
func (server *p2pServer) Sync(ctx context.Context, request *netproto.SyncRequest) (*netproto.SyncResponse, error) {
	if !server.acquire(ctx, server.syncs) {
		return nil, status.Error(codes.ResourceExhausted, "too many sync sessions")
	}
	defer server.release(server.syncs)

	limit := int(request.GetLimit())
	if limit == 0 {
		limit = 128
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 512 {
		limit = 512
	}
	payload := server.node.GetSyncPayload(request.GetFromIndex(), limit)
	// Canonical encoding matches Python json.dumps semantics for numbers, so
	// integral floats (e.g. the genesis timestamp 0.0) survive the wire.
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return nil, status.Error(codes.Internal, "sync payload encoding failed")
	}
	return &netproto.SyncResponse{Payload: encoded}, nil
}

// Bootstrap returns the seed's peer list plus its own signed record (the REST
// /bootstrap endpoint does the same), so a joining node learns the seed's
// advertised address as well.
func (server *p2pServer) Bootstrap(ctx context.Context, _ *netproto.Empty) (*netproto.Envelope, error) {
	peers := server.node.Peers()
	peers = append(peers, server.node.SelfPeerRecord())
	encoded, err := canonical.Marshal(map[string]any{"peers": peers})
	if err != nil {
		return nil, status.Error(codes.Internal, "bootstrap encoding failed")
	}
	return &netproto.Envelope{Payload: encoded}, nil
}

// Status returns chain id, height and finality information.
func (server *p2pServer) Status(ctx context.Context, _ *netproto.Empty) (*netproto.Envelope, error) {
	encoded, err := canonical.Marshal(server.node.PeerStatus())
	if err != nil {
		return nil, status.Error(codes.Internal, "status encoding failed")
	}
	return &netproto.Envelope{Payload: encoded}, nil
}

// BuildServer creates the gRPC server (one service bound to every P2P port).
func BuildServer(node NodeTransport, config NodeConfig) *grpc.Server {
	options := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(config.WSMaxSize),
		grpc.MaxSendMsgSize(config.WSMaxSize),
		grpc.ChainUnaryInterceptor(recoverUnaryInterceptor),
		grpc.ChainStreamInterceptor(recoverStreamInterceptor),
	}
	if credentials := node.TransportServerCredentials(); credentials != nil {
		options = append(options, grpc.Creds(credentials))
	}
	server := grpc.NewServer(options...)
	netproto.RegisterP2PServer(server, NewP2PServer(node))
	return server
}

// ---------------------------------------------------------------------- #
// Client side
// ---------------------------------------------------------------------- #

// PeerConnectionError is raised when a peer session cannot be opened.
type PeerConnectionError struct{ Message string }

func (e *PeerConnectionError) Error() string { return e.Message }

func channelOptions(peer map[string]any, maxSize int) []grpc.DialOption {
	options := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSize), grpc.MaxCallSendMsgSize(maxSize)),
	}
	if truthy(peer["tls"]) {
		options = append(options, grpc.WithAuthority(stringValue(peer["host"])))
	}
	return options
}

func dialAddress(peer map[string]any, portKey string) string {
	return stringValue(peer["host"]) + ":" + stringValue(peer[portKey])
}

func dialPeer(ctx context.Context, node NodeTransport, peer map[string]any, portKey string) (*grpc.ClientConn, error) {
	options := channelOptions(peer, node.WSMaxSize())
	if truthy(peer["tls"]) {
		credentials := node.TransportClientCredentials(peer)
		if credentials == nil {
			return nil, &PeerConnectionError{Message: "TLS requested but no client credentials"}
		}
		options = append(options, grpc.WithTransportCredentials(credentials))
	} else {
		options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return grpc.NewClient(dialAddress(peer, portKey), options...)
}

// OpenSession opens a gRPC session and completes the HELLO handshake.
func OpenSession(ctx context.Context, node NodeTransport, peer map[string]any, portKey string, timeout time.Duration) (*PeerStream, error) {
	address := dialAddress(peer, portKey)
	connection, err := dialPeer(ctx, node, peer, portKey)
	if err != nil {
		return nil, &PeerConnectionError{Message: fmt.Sprintf("%s: not reachable (%v)", address, err)}
	}

	// The stream must outlive this call (Python binds it to the caller's
	// context), so the RPC uses ctx; the timeout only gates the handshake.
	client := netproto.NewP2PClient(connection)
	stream, err := client.Session(ctx)
	if err != nil {
		connection.Close()
		return nil, &PeerConnectionError{Message: fmt.Sprintf("%s: not reachable (%v)", address, err)}
	}

	inbound := make(chan string, SessionQueueSize)
	outbound := make(chan string, SessionQueueSize)
	peerStream := newPeerStream(inbound, outbound, address)

	senderErr := make(chan error, 1)
	senderDone := make(chan struct{})
	reportSender := func(err error) {
		select {
		case senderErr <- err:
		default:
		}
	}
	go func() {
		defer close(senderDone)
		for {
			select {
			case message := <-outbound:
				if err := stream.Send(&netproto.Envelope{Payload: []byte(message)}); err != nil {
					reportSender(err)
					return
				}
			case <-peerStream.Done():
				// Flush messages queued before the close so a one-shot
				// session never drops its last message on the floor.
				for {
					select {
					case message := <-outbound:
						if err := stream.Send(&netproto.Envelope{Payload: []byte(message)}); err != nil {
							reportSender(err)
							return
						}
					default:
						reportSender(nil)
						return
					}
				}
			}
		}
	}()

	go func() {
		defer close(inbound)
		for {
			envelope, err := stream.Recv()
			if err != nil {
				if err != io.EOF && !IsPeerClosed(err) {
					log.Printf("client session reader ended: %v", err)
				}
				return
			}
			select {
			case inbound <- string(envelope.Payload):
			case <-peerStream.Done():
				return
			}
		}
	}()

	peerStream.closeCallback = func() {
		// Wait briefly for the queued messages to reach gRPC, but never
		// forever: a hostile peer that stops reading leaves the sender
		// blocked inside stream.Send (gRPC flow control). Tearing the
		// connection down unblocks it, so Close cannot leak a goroutine.
		// CloseSend is only called once the sender stopped; calling it
		// concurrently with an in-flight Send races inside gRPC.
		stuck := false
		select {
		case <-senderDone:
		case <-time.After(500 * time.Millisecond):
			stuck = true
		}
		if !stuck {
			_ = stream.CloseSend()
		}
		connection.Close()
		select {
		case <-senderDone:
		case <-time.After(time.Second):
		}
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hello, err := canonical.Marshal(node.HelloPayload("HELLO"))
	if err == nil {
		err = peerStream.Send(handshakeCtx, string(hello))
	}
	if err != nil {
		peerStream.Close()
		return nil, err
	}
	response, err := peerStream.Recv(handshakeCtx)
	if err != nil {
		peerStream.Close()
		return nil, err
	}
	decodedValue, decodeErr := canonical.Decode([]byte(response))
	decoded, ok := decodedValue.(map[string]any)
	if decodeErr != nil || !ok || !node.VerifyHello(decoded) {
		peerStream.Close()
		return nil, &PeerConnectionError{Message: "handshake rejected by peer"}
	}
	return peerStream, nil
}

// RemoteSync fetches one page of blocks from a peer over the Sync RPC.
func RemoteSync(ctx context.Context, node NodeTransport, peer map[string]any, fromIndex int64, limit int, timeout time.Duration) (map[string]any, error) {
	connection, err := dialPeer(ctx, node, peer, "p2p_port")
	if err != nil {
		return nil, err
	}
	defer connection.Close()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	response, err := netproto.NewP2PClient(connection).Sync(callCtx, &netproto.SyncRequest{FromIndex: fromIndex, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	value, err := canonical.Decode(response.Payload)
	if err != nil {
		return nil, err
	}
	payload, _ := value.(map[string]any)
	return payload, nil
}

// RemoteStatus asks a peer for its chain id, height and finality.
func RemoteStatus(ctx context.Context, node NodeTransport, peer map[string]any, timeout time.Duration) map[string]any {
	connection, err := dialPeer(ctx, node, peer, "p2p_port")
	if err != nil {
		return nil
	}
	defer connection.Close()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	response, err := netproto.NewP2PClient(connection).Status(callCtx, &netproto.Empty{})
	if err != nil {
		return nil
	}
	value, err := canonical.Decode(response.Payload)
	if err != nil {
		return nil
	}
	payload, _ := value.(map[string]any)
	return payload
}

// remoteBootstrapPeer asks a peer record for its bootstrap list over the
// transport configured for that record (TLS-aware), including the peer's own
// signed record.
func remoteBootstrapPeer(ctx context.Context, node NodeTransport, peer map[string]any, portKey string, timeout time.Duration) ([]any, error) {
	connection, err := dialPeer(ctx, node, peer, portKey)
	if err != nil {
		return nil, err
	}
	defer connection.Close()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	response, err := netproto.NewP2PClient(connection).Bootstrap(callCtx, &netproto.Empty{})
	if err != nil {
		return nil, err
	}
	value, err := canonical.Decode(response.Payload)
	if err != nil {
		return nil, err
	}
	payload, _ := value.(map[string]any)
	peers, _ := payload["peers"].([]any)
	return peers, nil
}

// RemoteBootstrap asks a seed for its peer list by address; the legacy helper
// stays plaintext (use remoteBootstrapPeer for TLS-aware dials).
func RemoteBootstrap(ctx context.Context, node NodeTransport, address string, timeout time.Duration) ([]any, error) {
	host, port, found := cutLast(address, ":")
	if !found {
		return nil, &PeerConnectionError{Message: fmt.Sprintf("invalid bootstrap address %q", address)}
	}
	return remoteBootstrapPeer(ctx, node, map[string]any{"host": host, "p2p_port": port}, "p2p_port", timeout)
}

func cutLast(text, separator string) (string, string, bool) {
	for i := len(text) - len(separator); i >= 0; i-- {
		if text[i:i+len(separator)] == separator {
			return text[:i], text[i+len(separator):], true
		}
	}
	return "", "", false
}
