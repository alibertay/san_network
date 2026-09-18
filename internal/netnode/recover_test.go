package netnode

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type panickingTransport struct{}

func (panickingTransport) PeerSession(context.Context, *PeerStream) error { panic("boom") }
func (panickingTransport) GetSyncPayload(int64, int) map[string]any       { return nil }
func (panickingTransport) Peers() []any                                   { return nil }
func (panickingTransport) PeerStatus() map[string]any                     { return nil }
func (panickingTransport) WSMaxSize() int                                 { return 1 << 20 }
func (panickingTransport) TransportServerCredentials() credentials.TransportCredentials {
	return nil
}
func (panickingTransport) TransportClientCredentials(map[string]any) credentials.TransportCredentials {
	return nil
}
func (panickingTransport) HelloPayload(string) map[string]any { return nil }
func (panickingTransport) VerifyHello(map[string]any) bool    { return false }
func (panickingTransport) SelfPeerRecord() map[string]any     { return nil }
func (panickingTransport) NoteInboundPeer(string)             {}

func TestRunPeerSessionRecoversPanic(t *testing.T) {
	server := &p2pServer{
		node:     panickingTransport{},
		sessions: make(chan struct{}, 1),
		syncs:    make(chan struct{}, 1),
	}
	stream := newPeerStream(make(chan string, 1), make(chan string, 1), "recover")
	defer stream.Close()
	if err := server.runPeerSession(context.Background(), stream); err == nil {
		t.Fatalf("a panicking PeerSession must return an error, not crash the node")
	}
}

func TestUnaryInterceptorRecoversPanic(t *testing.T) {
	response, err := recoverUnaryInterceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/san.test/Panic"},
		func(context.Context, any) (any, error) { panic("boom") })
	if err == nil {
		t.Fatalf("panicking unary handler returned %v without an error", response)
	}
}

type fakeServerStream struct{ grpc.ServerStream }

func TestStreamInterceptorRecoversPanic(t *testing.T) {
	err := recoverStreamInterceptor(nil, fakeServerStream{},
		&grpc.StreamServerInfo{FullMethod: "/san.test/PanicStream"},
		func(any, grpc.ServerStream) error { panic("boom") })
	if err == nil {
		t.Fatalf("panicking stream handler returned nil")
	}
}
