package netnode

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
)

// limitTestNode builds an unstarted node with the given configuration tweaks.
func limitTestNode(t *testing.T, tweak func(*NodeConfig)) *Node {
	t.Helper()
	config := testConfig()
	if tweak != nil {
		tweak(&config)
	}
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// TestMempoolLimitRejectsAndCounts is the section 13 invariant for the
// transaction pool: the cap rejects cleanly and increments its counter.
func TestMempoolLimitRejectsAndCounts(t *testing.T) {
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
	if got := metricValue(node, "mempool_rejected"); got != 1 {
		t.Fatalf("mempool_rejected = %d, want 1", got)
	}
	if got := node.PendingCount(); got != 1 {
		t.Fatalf("mempool grew to %d, want 1", got)
	}
}

// TestOrphanLimitEvictsAndCounts bounds the orphan buffer and records eviction.
func TestOrphanLimitEvictsAndCounts(t *testing.T) {
	node := limitTestNode(t, func(config *NodeConfig) { config.MaxOrphans = 2 })
	identity := mustIdentity(t)
	tip := node.blockchain.Tip()

	for index := 0; index < 5; index++ {
		block := ledger.NewBlock(tip.Index, fmt.Sprintf("%064x", index),
			identity.PublicKeyHex(), nil, []any{}, nowSeconds(), node.chainID, nil, 0, nil)
		block.ValidatorSignature = identity.SignHex([]byte(block.CurrentBlockHash))
		node.mu.Lock()
		node.processIncomingBlock(block)
		node.mu.Unlock()
	}
	if got := len(node.orphans); got > 2 {
		t.Fatalf("orphan buffer grew to %d, want <= 2", got)
	}
	if got := metricValue(node, "orphans_evicted"); got < 3 {
		t.Fatalf("orphans_evicted = %d, want >= 3", got)
	}
}

// TestBlockRequestLimitRejectsAndCounts caps the in-flight block-request table.
func TestBlockRequestLimitRejectsAndCounts(t *testing.T) {
	node := limitTestNode(t, func(config *NodeConfig) { config.MaxBlockRequests = 1 })
	node.mu.Lock()
	node.requestedBlocks["aaaaaaaa"] = struct{}{}
	node.mu.Unlock()

	if node.requestBlock(context.Background(), "bbbbbbbb") {
		t.Fatalf("request beyond the cap was accepted")
	}
	if got := metricValue(node, "block_requests_rejected"); got != 1 {
		t.Fatalf("block_requests_rejected = %d, want 1", got)
	}
	node.mu.Lock()
	got := len(node.requestedBlocks)
	node.mu.Unlock()
	if got != 1 {
		t.Fatalf("request table grew to %d, want 1", got)
	}
}

// TestStagedVoteHashCapRejectsAndCounts caps competing hashes per height.
func TestStagedVoteHashCapRejectsAndCounts(t *testing.T) {
	node := limitTestNode(t, nil)
	height := node.blockchain.Tip().Index + 1
	accepted := 0
	for index := 0; index < 12; index++ {
		identity, err := ledger.GenerateIdentity()
		if err != nil {
			t.Fatalf("GenerateIdentity: %v", err)
		}
		hash := fmt.Sprintf("%064x", index+1)
		before := metricValue(node, "staged_votes_rejected")
		node.handleFinalityVote(finalityVoteFor(t, node, identity, height, hash))
		if metricValue(node, "staged_votes_rejected") == before {
			accepted++
		}
	}
	if accepted != 8 {
		t.Fatalf("accepted %d staged hashes, want 8", accepted)
	}
	if got := metricValue(node, "staged_votes_rejected"); got < 4 {
		t.Fatalf("staged_votes_rejected = %d, want >= 4", got)
	}
	node.mu.Lock()
	hashes := len(node.finalityVotes[height])
	node.mu.Unlock()
	if hashes > 8 {
		t.Fatalf("staged hash table grew to %d, want <= 8", hashes)
	}
}

// TestPeerTableLimitRejectsAndCounts bounds the peer table.
func TestPeerTableLimitRejectsAndCounts(t *testing.T) {
	node := limitTestNode(t, func(config *NodeConfig) { config.MaxPeers = 1 })
	signer := mustIdentity(t)
	makePeer := func(host string, port int64) map[string]any {
		record, ok := CompletePeer(map[string]any{
			"host": host, "api_port": port, "chain_id": node.chainID,
			"genesis":    node.genesisFingerprint,
			"public_key": signer.PublicKeyHex(), "timestamp": nowSeconds(),
		}, node.config)
		if !ok {
			t.Fatalf("CompletePeer rejected the record")
		}
		payload, err := node.peerRecordPayload(record)
		if err != nil {
			t.Fatalf("peerRecordPayload: %v", err)
		}
		record["signature"] = signer.SignHex(payload)
		return record
	}
	if node.addPeer(makePeer("10.42.0.1", 9101)) != 1 {
		t.Fatalf("first peer was not added")
	}
	if node.addPeer(makePeer("10.42.0.2", 9102)) != 0 {
		t.Fatalf("peer beyond MaxPeers was added")
	}
	if got := metricValue(node, "peers_rejected_table"); got != 1 {
		t.Fatalf("peers_rejected_table = %d, want 1", got)
	}
	if got := len(node.Peers()); got != 1 {
		t.Fatalf("peer table grew to %d, want 1", got)
	}
}

// TestSeenVoteCacheStaysBounded pins the dedup cache reset path: it is capped
// and the reset is counted instead of growing without bound.
func TestSeenVoteCacheStaysBounded(t *testing.T) {
	node := limitTestNode(t, nil)
	for index := 0; index < 9000; index++ {
		node.broadcastVote(map[string]any{
			"height":     int64(index % 64),
			"block_hash": fmt.Sprintf("%064x", index),
			"public_key": fmt.Sprintf("%064x", index+1),
		})
	}
	if got := metricValue(node, "vote_seen_cache_resets"); got < 1 {
		t.Fatalf("vote_seen_cache_resets = %d, want >= 1", got)
	}
	node.mu.Lock()
	size := len(node.seenVotes)
	node.mu.Unlock()
	if size > 8192 {
		t.Fatalf("seen vote cache grew to %d, want <= 8192", size)
	}
}
