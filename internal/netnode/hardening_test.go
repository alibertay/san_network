package netnode

import (
	"context"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
)

func guardNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

// newDispatchNode builds an unstarted node for message-dispatch tests.
func newDispatchNode(t *testing.T) *Node {
	t.Helper()
	node, err := NewNode(testConfig(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

func newTestStream() *PeerStream {
	return newPeerStream(
		make(chan string, SessionQueueSize),
		make(chan string, SessionQueueSize),
		"test-session",
	)
}

func TestDispatchPeerMessageNeverPanics(t *testing.T) {
	node := newDispatchNode(t)
	messages := []string{
		"",
		"null",
		"[]",
		"{",
		"\x00\xff",
		`{"type":1}`,
		`{"type":"BLOCK"}`,
		`{"type":"BLOCK","block":1}`,
		`{"type":"BLOCK","block":{}}`,
		`{"type":"BLOCK","block":{"index":1,"transactions":"x"}}`,
		`{"type":"GET_BLOCK"}`,
		`{"type":"GET_BLOCK","block_hash":1}`,
		`{"type":"GET_BLOCK","block_hash":"aa"}`,
		`{"type":"BLOCK_VOTE_REQUEST"}`,
		`{"type":"BLOCK_VOTE_REQUEST","block":{"index":"x"}}`,
		`{"type":"PING"}`,
		`{"type":"TX"}`,
		`{"type":"TX","tx":"string"}`,
		`{"type":"TX","tx":{"chain_id":1,"sender":[],"nonce":{}}}`,
		`{"type":"TXS","txs":"not-a-list"}`,
		`{"type":"TXS","txs":[null,1,"x",{"sender":"zz"}]}`,
		`{"type":"FINALITY_VOTE","vote":[]}`,
		`{"type":"FINALITY_VOTE","vote":{"chain_id":"san-devnet-1","height":"x"}}`,
		`{"type":"FINALITY_VOTE","vote":{"chain_id":"san-devnet-1","height":1,"public_key":1}}`,
		`{"type":"PEER_UPDATE","peer":[]}`,
		`{"type":"PEER_UPDATE","peer":{"host":1}}`,
		`{"type":"PEER_UPDATE","peer":{"host":"127.0.0.1","api_port":{}}}`,
		`{"type":"DEAD_PEER","peer":1}`,
		`{"type":"GET_PEERS"}`,
		`{"type":"PEERS","peers":[1,2,3]}`,
		`{"type":"PEERS","peers":[{"host":"127.0.0.1","api_port":"x"}]}`,
		`{"type":"BLOCK_NOT_FOUND"}`,
		`{"type":"BLOCK_NOT_FOUND","block_hash":[]}`,
		`{"type":"UNKNOWN"}`,
	}
	ctx := context.Background()
	for index, message := range messages {
		message := message
		stream := newTestStream()
		guardNoPanic(t, "dispatchPeerMessage", func() {
			_ = node.dispatchPeerMessage(ctx, stream, message)
		})
		stream.Close()
		_ = index
	}
}

func TestPeerHandshakeVerifiersNeverPanic(t *testing.T) {
	node := newDispatchNode(t)
	records := []map[string]any{
		nil,
		{},
		{"type": "HELLO"},
		{"type": "HELLO", "protocol": "x", "chain_id": []any{}, "timestamp": map[string]any{}},
		{"type": "HELLO", "protocol": int64(ProtocolVersion), "chain_id": node.chainID,
			"timestamp": int64(0), "public_key": 1, "signature": 2},
	}
	for index, record := range records {
		guardNoPanic(t, "VerifyHello", func() { _ = node.VerifyHello(record) })
		guardNoPanic(t, "VerifyPeerRecord", func() { _ = node.VerifyPeerRecord(record) })
		_ = index
	}

	peers := []any{nil, int64(1), "peer", map[string]any{}, map[string]any{"host": []any{}}, []any{1, 2, 3}}
	for index, peer := range peers {
		guardNoPanic(t, "CompletePeer", func() { _, _ = CompletePeer(peer, node.config) })
		guardNoPanic(t, "addPeer", func() { _ = node.addPeer(peer) })
		_ = index
	}
}

func TestHostilePeerListIsBounded(t *testing.T) {
	node := newDispatchNode(t)
	node.config.MaxPeers = 4
	node.config.RequireBlockSig = false
	peers := make([]any, 0, 1000)
	for index := 0; index < 1000; index++ {
		peers = append(peers, map[string]any{
			"host":     "10.0.0.1",
			"api_port": int64(index),
			"chain_id": node.chainID,
		})
	}
	guardNoPanic(t, "handlePeersMessage", func() { _ = node.handlePeersMessage(peers) })
	if len(node.Peers()) > node.config.MaxPeers {
		t.Fatalf("peer table grew beyond MaxPeers: %d", len(node.Peers()))
	}
}

func TestMempoolCapRejectsWhenFull(t *testing.T) {
	identity := mustIdentity(t)
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	config := testConfig()
	config.GenesisAllocations = map[string]int64{address: 100 * ledger.SANBase}
	config.BlockThresholdFee = 1e9
	config.MaxMempool = 1
	node, err := NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	sign := func(nonce int64) map[string]any {
		payload := map[string]any{
			"chain_id": "san-devnet-1",
			"sender":   identity.PublicKeyHex(),
			"nonce":    nonce,
			"receiver": "0x" + strings.Repeat("cd", 20),
			"value":    "1",
		}
		signature, err := ledger.SignPayload(payload, identity.PrivateKey)
		if err != nil {
			t.Fatalf("SignPayload: %v", err)
		}
		payload["signature"] = signature
		return payload
	}

	if _, err := node.SubmitTransaction(sign(0)); err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	if _, err := node.SubmitTransaction(sign(1)); err == nil {
		t.Fatalf("expected the mempool cap to reject the second transaction")
	}
}

func TestBlockTimestampMustBeNumeric(t *testing.T) {
	node := newDispatchNode(t)
	tip := node.blockchain.Tip()
	block := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, "UNSIGNED_VALIDATOR", nil,
		[]any{}, "not-a-number", node.chainID, nil, 0, nil)
	if node.VerifyBlock(block, false) {
		t.Fatalf("block with a non-numeric timestamp was accepted")
	}
}

func FuzzDispatchPeerMessage(f *testing.F) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		f.Fatalf("GenerateIdentity: %v", err)
	}
	node, err := NewNode(testConfig(), identity)
	if err != nil {
		f.Fatalf("NewNode: %v", err)
	}
	seeds := []string{
		`{"type":"PING"}`,
		`{"type":"BLOCK","block":{}}`,
		`{"type":"TX","tx":{}}`,
		`{"type":"PEERS","peers":[]}`,
		`{"type":"FINALITY_VOTE","vote":{}}`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, message string) {
		stream := newTestStream()
		defer stream.Close()
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("dispatchPeerMessage(%q) panicked: %v", message, recovered)
			}
		}()
		_ = node.dispatchPeerMessage(context.Background(), stream, message)
	})
}
