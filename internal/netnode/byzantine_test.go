package netnode

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
)

// stateFingerprint is the subset of canonical node state hostile input must
// never change.
type stateFingerprint struct {
	tipHash         string
	finalizedHeight int64
	finalizedHash   string
	peers           int
	mempool         int
	validators      int
	chainLength     int
}

func fingerprint(node *Node) stateFingerprint {
	node.mu.Lock()
	defer node.mu.Unlock()
	return stateFingerprint{
		tipHash:         node.blockchain.Tip().CurrentBlockHash,
		finalizedHeight: node.finalizedHeight,
		finalizedHash:   node.finalizedHash,
		peers:           len(node.PEERS),
		mempool:         len(node.transactionPool),
		validators:      len(node.blockchain.ActiveValidators()),
		chainLength:     len(node.blockchain.Chain),
	}
}

// TestByzantineMalformedMessagesDoNotChangeState feeds hostile envelopes
// through the peer dispatcher and pins that no panic happens and no chain,
// peer, mempool or finality state changes.
func TestByzantineMalformedMessagesDoNotChangeState(t *testing.T) {
	node := newDispatchNode(t)
	messages := []string{
		"",
		"null",
		"true",
		"42",
		"[",
		`{"type":`,
		"\x00\xff\xfe\x01",
		strings.Repeat("A", 1<<20),
		`{"type":{"nested":true}}`,
		`{"type":"BLOCK","block":{"index":"x"}}`,
		`{"type":"BLOCK","block":{"index":9999999999999999999999}}`,
		`{"type":"BLOCK","block":"not-an-object"}`,
		`{"type":"FINALITY_VOTE","vote":{"chain_id":"san-devnet-1","height":1,"public_key":"00","signature":"00","block_hash":"aa"}}`,
		`{"type":"TX","tx":{"chain_id":"san-devnet-1"}}`,
		`{"type":"TXS","txs":[null,0,"x",[]]}`,
		`{"type":"PEERS","peers":[{"host":"10.0.0.1"},{"host":[]}]}`,
		`{"type":"PEER_UPDATE","peer":{"host":"10.0.0.1","api_port":"x"}}`,
		`{"type":"DEAD_PEER","peer":{"host":"10.0.0.1"}}`,
		`{"type":"GET_BLOCK","block_hash":"zz"}`,
		`{"type":"BLOCK_VOTE_REQUEST","block":{"index":"x"}}`,
		`{"type":"UNKNOWN","x":[1,2,3]}`,
	}
	before := fingerprint(node)
	ctx := context.Background()
	for index, message := range messages {
		message := message
		stream := newTestStream()
		guardNoPanic(t, fmt.Sprintf("dispatch#%d", index), func() {
			_ = node.dispatchPeerMessage(ctx, stream, message)
		})
		stream.Close()
	}
	after := fingerprint(node)
	if before != after {
		t.Fatalf("hostile messages changed node state:\n before=%+v\n after =%+v", before, after)
	}
	node.mu.Lock()
	orphans := len(node.orphans)
	node.mu.Unlock()
	if orphans != 0 {
		t.Fatalf("malformed messages created %d orphan(s)", orphans)
	}
	if node.PendingCount() != 0 {
		t.Fatalf("mempool is not empty: %d", node.PendingCount())
	}
}

// TestByzantineHandshakeRejections pins the protocol/chain/freshness/signature
// checks on HELLO and HELLO_ACK records.
func TestByzantineHandshakeRejections(t *testing.T) {
	node := newDispatchNode(t)
	valid := node.HelloPayload("HELLO")
	if !node.VerifyHello(valid) {
		t.Fatalf("self-authored HELLO did not verify")
	}

	wrongProtocol := deepCopyStringMap(valid)
	wrongProtocol["protocol"] = int64(ProtocolVersion + 1)
	if node.VerifyHello(wrongProtocol) {
		t.Fatalf("wrong protocol version was accepted")
	}
	wrongChain := deepCopyStringMap(valid)
	wrongChain["chain_id"] = "other-chain"
	if node.VerifyHello(wrongChain) {
		t.Fatalf("wrong chain id was accepted")
	}
	stale := deepCopyStringMap(valid)
	stale["timestamp"] = nowSeconds() - HelloTTL - 30
	stale["signature"] = node.identity.SignHex(mustCanonical(t, mapWithout(stale, helloMetaFields)))
	if node.VerifyHello(stale) {
		t.Fatalf("stale HELLO was accepted")
	}
	tampered := deepCopyStringMap(valid)
	tampered["public_key"] = "00"
	if node.VerifyHello(tampered) {
		t.Fatalf("tampered public key was accepted")
	}
	unsigned := deepCopyStringMap(valid)
	delete(unsigned, "signature")
	if node.VerifyHello(unsigned) {
		t.Fatalf("unsigned HELLO was accepted")
	}
	wrongType := deepCopyStringMap(valid)
	wrongType["type"] = "HELLO_ACK"
	wrongType["signature"] = node.identity.SignHex(mustCanonical(t, mapWithout(wrongType, helloMetaFields)))
	if !node.VerifyHello(wrongType) {
		t.Fatalf("valid HELLO_ACK was rejected")
	}
	if node.VerifyHello(nil) || node.VerifyHello(map[string]any{}) {
		t.Fatalf("nil/empty HELLO was accepted")
	}
}

func mustCanonical(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := canonical.Marshal(value)
	if err != nil {
		t.Fatalf("canonical.Marshal: %v", err)
	}
	return encoded
}

// TestByzantineOrphanFloodIsBounded buffers hundreds of branches and checks
// the orphan buffer and the canonical chain stay bounded and unchanged.
func TestByzantineOrphanFloodIsBounded(t *testing.T) {
	node := newDispatchNode(t)
	identity := mustIdentity(t)
	tip := node.blockchain.Tip()
	before := fingerprint(node)

	for index := 0; index < 300; index++ {
		block := ledger.NewBlock(tip.Index, fmt.Sprintf("%064x", index),
			identity.PublicKeyHex(), nil, []any{}, nowSeconds(), node.chainID, nil, 0, nil)
		block.ValidatorSignature = identity.SignHex([]byte(block.CurrentBlockHash))
		node.mu.Lock()
		node.processIncomingBlock(block)
		node.mu.Unlock()
	}
	if got := len(node.orphans); got > node.config.MaxOrphans {
		t.Fatalf("orphan buffer grew to %d, want <= %d", got, node.config.MaxOrphans)
	}
	if after := fingerprint(node); after != before {
		t.Fatalf("orphan flood changed canonical state:\n before=%+v\n after =%+v", before, after)
	}
}

// TestByzantineVoteBufferIsBounded stages far more votes than the protocol
// allows and checks every staging bound holds.
func TestByzantineVoteBufferIsBounded(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	height := node.blockchain.Tip().Index + 1

	// One hash with more distinct fake voters than MaxStagedVotersPerHash.
	saturating := make([]*ledger.NodeIdentity, MaxStagedVotersPerHash+12)
	for i := range saturating {
		saturating[i] = mustIdentity(t)
	}
	primaryHash := strings.Repeat("00", 32)
	for _, identity := range saturating {
		node.handleFinalityVote(finalityVoteFor(t, node, identity, height, primaryHash))
	}
	// Additional hashes with disjoint fake voters fill the per-height cap.
	extraVoters := make([]*ledger.NodeIdentity, 0, 64)
	for i := 0; i < 64; i++ {
		extraVoters = append(extraVoters, mustIdentity(t))
	}
	for hashIndex := 1; hashIndex < 32; hashIndex++ {
		hash := strings.Repeat(fmt.Sprintf("%02x", hashIndex), 32)
		window := extraVoters[(hashIndex-1)*2 : hashIndex*2]
		for _, identity := range window {
			node.handleFinalityVote(finalityVoteFor(t, node, identity, height, hash))
		}
	}

	node.mu.Lock()
	hashes := len(node.finalityVotes[height])
	stagedPrimary := len(node.finalityVotes[height][primaryHash])
	total := 0
	for _, byHash := range node.finalityVotes {
		total += len(byHash)
		for _, voters := range byHash {
			if len(voters) > MaxStagedVotersPerHash {
				node.mu.Unlock()
				t.Fatalf("staged voters per hash exceeded %d", MaxStagedVotersPerHash)
			}
		}
	}
	node.mu.Unlock()
	if stagedPrimary != MaxStagedVotersPerHash {
		t.Fatalf("primary hash staged %d voters, want the %d cap", stagedPrimary, MaxStagedVotersPerHash)
	}
	if hashes > 8 {
		t.Fatalf("staged hashes at one height: %d, want <= 8", hashes)
	}
	if total > VoteLookahead*4 {
		t.Fatalf("staged hashes across heights: %d, want <= %d", total, VoteLookahead*4)
	}
	if node.FinalizedHeight() != 0 {
		t.Fatalf("staged junk votes advanced finality")
	}
}

// TestByzantineBlockTimeAndBranchRejections covers future/old/duplicate/branch
// blocks and the DEAD_PEER eviction claim.
func TestByzantineBlockTimeAndBranchRejections(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node

	future := buildProposal(t, node, vs.ids[0], nil, 0)
	future.Timestamp = nowSeconds() + BlockFutureDrift + 120
	future.CurrentBlockHash = future.CalculateHash()
	future.ValidatorSignature = vs.ids[0].SignHex([]byte(future.CurrentBlockHash))
	if node.VerifyBlock(future, false) {
		t.Fatalf("far-future block was accepted")
	}

	old := buildProposal(t, node, vs.ids[0], nil, 0)
	old.Timestamp = nowSeconds() - BlockPastDrift - 120
	old.CurrentBlockHash = old.CalculateHash()
	old.ValidatorSignature = vs.ids[0].SignHex([]byte(old.CurrentBlockHash))
	if node.VerifyBlock(old, false) {
		t.Fatalf("backdated live block was accepted")
	}

	valid := commitProposal(t, node, vs.ids[0], nil)
	if ingestBlock(t, node, valid) {
		t.Fatalf("duplicate block was accepted")
	}

	futureHeight := buildProposal(t, node, vs.ids[0], nil, 0)
	futureHeight.Index = valid.Index + 5
	futureHeight.PreviousBlockHash = strings.Repeat("ab", 32)
	futureHeight.CurrentBlockHash = futureHeight.CalculateHash()
	futureHeight.ValidatorSignature = vs.ids[0].SignHex([]byte(futureHeight.CurrentBlockHash))
	before := fingerprint(node)
	node.mu.Lock()
	buffered := node.processIncomingBlock(futureHeight)
	node.mu.Unlock()
	if !buffered {
		t.Fatalf("future-height branch was not buffered for later assembly")
	}
	if after := fingerprint(node); after.chainLength != before.chainLength || after.tipHash != before.tipHash {
		t.Fatalf("future-height block changed the canonical chain")
	}

	// An unsigned eviction claim is ignored.
	peer := map[string]any{"host": "10.9.9.9", "api_port": int64(1234)}
	node.mu.Lock()
	node.PEERS = append(node.PEERS, peer)
	node.mu.Unlock()
	message, _ := encodeObject(map[string]any{"type": "DEAD_PEER", "peer": peer})
	stream := newTestStream()
	_ = node.dispatchPeerMessage(context.Background(), stream, string(message))
	stream.Close()
	if len(node.Peers()) == 0 {
		t.Fatalf("DEAD_PEER claim evicted a locally tracked peer")
	}
}

// TestByzantinePeerRecordFloodKeepsTableBounded checks that repeated hostile
// peer advertisements cannot fill the peer table past the subnet cap.
func TestByzantinePeerRecordFloodKeepsTableBounded(t *testing.T) {
	config := testConfig()
	config.RequireBlockSig = false
	config.MaxPeers = 64
	config.MaxPeersPerSubnet = 4
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	records := make([]any, 0, 200)
	for index := 0; index < 200; index++ {
		records = append(records, map[string]any{
			"chain_id": node.chainID,
			"host":     "10.2.2.2",
			"api_port": int64(20000 + index),
		})
	}
	guardNoPanic(t, "peer flood", func() { node.handlePeersMessage(records) })
	if got := len(node.Peers()); got > config.MaxPeersPerSubnet {
		t.Fatalf("peer table grew to %d, want <= %d", got, config.MaxPeersPerSubnet)
	}
	if got := metricValue(node, "peers_rejected_subnet"); got == 0 {
		t.Fatalf("subnet rejection metric was not incremented")
	}
}

// TestByzantineFakeValidatorVotes checks votes from keys outside the active
// set, wrong chains, far heights and duplicates are all rejected with metrics.
func TestByzantineFakeValidatorVotes(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	outsider := mustIdentity(t)
	beforeVoter := metricValue(node, "votes_dropped_voter")
	node.handleFinalityVote(finalityVoteFor(t, node, outsider, block.Index, block.CurrentBlockHash))
	if got := metricValue(node, "votes_dropped_voter"); got != beforeVoter+1 {
		t.Fatalf("fake validator vote was not dropped: %d -> %d", beforeVoter, got)
	}

	wrongChain := finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash)
	wrongChain["chain_id"] = "other-chain"
	beforeInvalid := metricValue(node, "votes_dropped_invalid")
	node.handleFinalityVote(wrongChain)
	if got := metricValue(node, "votes_dropped_invalid"); got != beforeInvalid+1 {
		t.Fatalf("wrong-chain vote was not dropped: %d -> %d", beforeInvalid, got)
	}

	badSignature := finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash)
	badSignature["signature"] = strings.Repeat("00", 64)
	node.handleFinalityVote(badSignature)
	if got := metricValue(node, "votes_dropped_invalid"); got != beforeInvalid+2 {
		t.Fatalf("bad-signature vote was not dropped: %d", got)
	}

	vote := finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash)
	node.handleFinalityVote(vote)
	beforeDuplicate := metricValue(node, "votes_dropped_duplicate")
	node.handleFinalityVote(vote)
	if got := metricValue(node, "votes_dropped_duplicate"); got != beforeDuplicate+1 {
		t.Fatalf("duplicate vote was not counted as duplicate: %d", got)
	}

	farAhead := finalityVoteFor(t, node, vs.ids[0], node.blockchain.Tip().Index+VoteLookahead+10, strings.Repeat("cd", 32))
	beforeHeight := metricValue(node, "votes_dropped_height")
	node.handleFinalityVote(farAhead)
	if got := metricValue(node, "votes_dropped_height"); got != beforeHeight+1 {
		t.Fatalf("far-ahead vote was not dropped: %d", got)
	}

	// A vote from a real validator for a non-canonical hash is staged, never
	// finalized.
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, strings.Repeat("ef", 32)))
	if node.FinalizedHeight() != 0 {
		t.Fatalf("staged votes advanced finality without a canonical quorum")
	}
}
