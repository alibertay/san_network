package netnode

import (
	"context"
	"testing"
	"time"
)

// peerTestNode builds a memory-backed node with a distinct API port so its
// signed record is not treated as "self" by another test node.
func peerTestNode(t *testing.T, apiPort int) *Node {
	t.Helper()
	config := testConfig()
	config.APIPort = apiPort
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// resignRecord re-signs a record after a test mutates fields covered by the
// signature (e.g. the timestamp), so the record exercises the intended check
// instead of failing on the signature first.
func resignRecord(t *testing.T, node *Node, record map[string]any) map[string]any {
	t.Helper()
	payload, err := node.peerRecordPayload(record)
	if err != nil {
		t.Fatalf("peerRecordPayload: %v", err)
	}
	record["signature"] = node.identity.SignHex(payload)
	return record
}

// TestPeersReplyMergesVerifiedRecords dispatches a PEERS message through the
// generic peer handler and checks that signed records are merged while
// tampered, stale and self records are rejected.
func TestPeersReplyMergesVerifiedRecords(t *testing.T) {
	local := peerTestNode(t, 18101)
	remote := peerTestNode(t, 18102)

	valid := remote.SelfPeerRecord()
	tampered := deepCopyStringMap(valid)
	tampered["host"] = "10.9.9.9"

	stale := remote.SelfPeerRecord()
	stale["timestamp"] = nowSeconds() - local.config.PeerRecordTTL - 60
	resignRecord(t, remote, stale)

	self := local.SelfPeerRecord()

	message, err := encodeObject(map[string]any{
		"type":  "PEERS",
		"peers": []any{valid, tampered, stale, self},
	})
	if err != nil {
		t.Fatalf("encodeObject: %v", err)
	}
	if err := local.dispatchPeerMessage(context.Background(), nil, string(message)); err != nil {
		t.Fatalf("dispatchPeerMessage(PEERS): %v", err)
	}

	if len(local.PEERS) != 1 {
		t.Fatalf("expected exactly one merged peer, got %d: %v", len(local.PEERS), local.PEERS)
	}
	if !SamePeer(local.PEERS[0], valid) {
		t.Errorf("merged peer does not match the valid record: %v", local.PEERS[0])
	}
}

// TestGetPeersRepliesWithSignedRecords checks the responder side over an
// in-memory session: GET_PEERS yields a PEERS message whose records verify.
func TestGetPeersRepliesWithSignedRecords(t *testing.T) {
	local := peerTestNode(t, 18103)
	remote := peerTestNode(t, 18104)
	if added := local.AddPeer(remote.SelfPeerRecord()); added != 1 {
		t.Fatalf("AddPeer: expected 1, got %d", added)
	}

	stream := newPeerStream(make(chan string, 4), make(chan string, 4), "test")
	message, err := encodeObject(map[string]any{"type": "GET_PEERS"})
	if err != nil {
		t.Fatalf("encodeObject: %v", err)
	}
	if err := local.handlePeerMessage(context.Background(), stream, string(message)); err != nil {
		t.Fatalf("handlePeerMessage(GET_PEERS): %v", err)
	}

	select {
	case raw := <-stream.outbound:
		data, err := decodeObject(raw)
		if err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		if messageType, _ := data["type"].(string); messageType != "PEERS" {
			t.Fatalf("expected PEERS reply, got %v", data)
		}
		peers, _ := data["peers"].([]any)
		if len(peers) != 1 {
			t.Fatalf("expected one peer in the reply, got %v", data["peers"])
		}
		record, _ := peers[0].(map[string]any)
		if !local.VerifyPeerRecord(record) {
			t.Errorf("reply contains an unverifiable record: %v", record)
		}
	default:
		t.Fatalf("GET_PEERS produced no reply")
	}
}

// TestBlockNotFoundMarksPeers verifies the reply classifier, the missing-block
// ordering (marked peers are tried last, but never dropped) and the dispatch of
// an unsolicited BLOCK_NOT_FOUND.
func TestBlockNotFoundMarksPeers(t *testing.T) {
	local := peerTestNode(t, 18105)
	remote := peerTestNode(t, 18106)

	peerA := remote.SelfPeerRecord()
	peerB := deepCopyStringMap(peerA)
	peerB["api_port"] = int64(18107)
	peerB["peer_port"] = int64(18108)
	resignRecord(t, remote, peerB)

	if added := local.AddPeer(peerA); added != 1 {
		t.Fatalf("AddPeer(peerA): expected 1, got %d", added)
	}
	if added := local.AddPeer(peerB); added != 1 {
		t.Fatalf("AddPeer(peerB): expected 1, got %d", added)
	}

	blockHash := "aa" + "00"
	before := local.blockFetchPeers(blockHash)
	if len(before) != 2 {
		t.Fatalf("expected 2 fetch candidates, got %d", len(before))
	}

	missing := before[0]
	local.markPeerMissingBlock(missing, blockHash)
	if !local.peerMissingBlock(missing, blockHash) {
		t.Fatalf("marked peer is not reported as missing the block")
	}
	after := local.blockFetchPeers(blockHash)
	if len(after) != 2 {
		t.Fatalf("marked peer must still be tried as a last resort, got %d candidates", len(after))
	}
	if !SamePeer(after[1], missing) {
		t.Errorf("marked peer was not moved to the back: %v", after)
	}
	local.clearBlockMisses(blockHash)
	if local.peerMissingBlock(missing, blockHash) {
		t.Errorf("clearBlockMisses did not drop the mark")
	}

	if block, notFound, ok := classifyBlockReply(map[string]any{"type": "BLOCK_NOT_FOUND", "block_hash": blockHash}); !ok || !notFound || block != nil {
		t.Errorf("BLOCK_NOT_FOUND classified as block=%v notFound=%v ok=%v", block, notFound, ok)
	}
	if _, notFound, ok := classifyBlockReply(map[string]any{"type": "TXS"}); ok || notFound {
		t.Errorf("an unrelated reply must not classify as BLOCK/NOT_FOUND")
	}
	if err := local.dispatchPeerMessage(context.Background(), nil, `{"type":"BLOCK_NOT_FOUND","block_hash":"`+blockHash+`"}`); err != nil {
		t.Errorf("unsolicited BLOCK_NOT_FOUND dispatch failed: %v", err)
	}
}

// TestPeersExchangeOverSession exercises the real gRPC path: B learns C from
// A's PEERS reply to B's GET_PEERS, and an unknown GET_BLOCK is answered with
// BLOCK_NOT_FOUND.
func TestPeersExchangeOverSession(t *testing.T) {
	ports := freePorts(t, 9)

	nodeA, err := NewNode(func() NodeConfig {
		config := testConfig()
		config.APIPort = ports[0]
		config.PeerPort = ports[1]
		config.P2PPort = ports[2]
		config.ControllerPort = ports[3]
		return config
	}(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode A: %v", err)
	}
	nodeB, err := NewNode(func() NodeConfig {
		config := testConfig()
		config.APIPort = ports[4]
		return config
	}(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode B: %v", err)
	}
	nodeC, err := NewNode(func() NodeConfig {
		config := testConfig()
		config.APIPort = ports[5]
		return config
	}(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode C: %v", err)
	}

	if err := nodeA.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node A gRPC ports: %v", err)
	}
	defer nodeA.Stop()

	if added := nodeA.AddPeer(nodeC.SelfPeerRecord()); added != 1 {
		t.Fatalf("A.AddPeer(C): expected 1, got %d", added)
	}
	if added := nodeB.AddPeer(nodeA.SelfPeerRecord()); added != 1 {
		t.Fatalf("B.AddPeer(A): expected 1, got %d", added)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if added := nodeB.requestPeers(ctx); added != 1 {
		t.Fatalf("requestPeers: expected 1 new peer, got %d (peers=%v)", added, nodeB.Peers())
	}
	if len(nodeB.PEERS) != 2 {
		t.Fatalf("expected B to know A and C, got %v", nodeB.Peers())
	}

	stream, err := OpenSession(ctx, nodeB, nodeA.SelfPeerRecord(), "p2p_port", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer stream.Close()
	if err := stream.Send(ctx, `{"type":"GET_BLOCK","block_hash":"00000000000000000000000000000000000000000000000000000000000000ff"}`); err != nil {
		t.Fatalf("send GET_BLOCK: %v", err)
	}
	raw, err := stream.Recv(ctx)
	if err != nil {
		t.Fatalf("receive block reply: %v", err)
	}
	reply, err := decodeObject(raw)
	if err != nil {
		t.Fatalf("decode block reply: %v", err)
	}
	if messageType, _ := reply["type"].(string); messageType != "BLOCK_NOT_FOUND" {
		t.Fatalf("expected BLOCK_NOT_FOUND, got %v", reply)
	}
}
