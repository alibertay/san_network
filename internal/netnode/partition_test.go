package netnode

import (
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
)

// Deterministic, in-process partition tests. Blocks and votes are injected
// through the same seams the P2P dispatch uses (processIncomingBlock,
// handleFinalityVote), so the scenarios exercise fork choice, finality and
// mempool recovery without sockets or sleeps. Partition membership is a test
// concept only: production gossip has no membership list.
//
// Expected behavior for every scenario:
//   - a side with less than 2/3 of the active stake can never finalize;
//   - long-lived partitions may hold competing branches, but healing always
//     converges on the longest verified branch;
//   - finality is monotone: a finalized checkpoint hash is never rewritten;
//   - state roots, balances and nonces converge with the canonical chain and a
//     transaction executes at most once.

type partitionCluster struct {
	t           *testing.T
	ids         []*ledger.NodeIdentity
	addrs       []string
	nodes       []*Node
	allocations map[string]int64
}

// newPartitionCluster builds count nodes sharing one genesis allocation and
// one deposit block, so every node starts at the same height with the same
// active validator set.
func newPartitionCluster(t *testing.T, count int) *partitionCluster {
	t.Helper()
	ids := make([]*ledger.NodeIdentity, count)
	for i := range ids {
		ids[i] = mustIdentity(t)
	}
	config := validatorSetConfig(t, ids)

	first, err := NewNode(config, ids[0])
	if err != nil {
		t.Fatalf("NewNode(first): %v", err)
	}
	txs := make([]any, 0, count)
	for _, identity := range ids {
		txs = append(txs, signTransaction(t, first, identity, map[string]any{
			"chain_id": first.chainID,
			"sender":   identity.PublicKeyHex(),
			"nonce":    int64(0),
			"validator": map[string]any{
				"command": "deposit",
				"amount":  testStakeUnits,
			},
		}).Payload)
	}
	deposit := buildProposal(t, first, ids[0], txs, -1)
	first.mu.Lock()
	if !first.commitBlock(deposit, true) {
		first.mu.Unlock()
		t.Fatalf("deposit block failed to commit on the first node")
	}
	first.mu.Unlock()

	cluster := &partitionCluster{
		t:           t,
		ids:         ids,
		addrs:       make([]string, count),
		nodes:       []*Node{first},
		allocations: config.GenesisAllocations,
	}
	for i, identity := range ids {
		cluster.addrs[i] = identityAddress(t, identity)
	}
	for i := 1; i < count; i++ {
		node, err := NewNode(config, ids[i])
		if err != nil {
			t.Fatalf("NewNode(%d): %v", i, err)
		}
		node.mu.Lock()
		if !node.commitBlock(deposit, true) {
			node.mu.Unlock()
			t.Fatalf("deposit block failed to replay on node %d", i)
		}
		node.mu.Unlock()
		cluster.nodes = append(cluster.nodes, node)
	}
	return cluster
}

func (c *partitionCluster) accept(target int, block *ledger.Block) bool {
	node := c.nodes[target]
	node.mu.Lock()
	accepted := node.processIncomingBlock(block)
	node.mu.Unlock()
	return accepted
}

// proposerIn picks the partition member with the lowest expected round for the
// next height, so a side can always act within MaxProposerRounds.
func (c *partitionCluster) proposerIn(members []int) (identityIndex int, round int64) {
	node := c.nodes[members[0]]
	identityIndex, round = -1, -1
	for _, member := range members {
		candidate := proposalRoundFor(node, c.addrs[member])
		if candidate < 0 {
			continue
		}
		if identityIndex == -1 || candidate < round {
			identityIndex, round = member, candidate
		}
	}
	if identityIndex == -1 {
		c.t.Fatalf("no partition member can propose")
	}
	return identityIndex, round
}

// buildAndDeliver builds a block on builder and delivers it to targets.
func (c *partitionCluster) buildAndDeliver(builder, proposer int, txs []any, round int64, targets []int) *ledger.Block {
	block := buildProposal(c.t, c.nodes[builder], c.ids[proposer], txs, round)
	for _, target := range targets {
		c.accept(target, block)
	}
	return block
}

// castVotes signs a vote with every voter identity and delivers it to targets.
func (c *partitionCluster) castVotes(voters []int, height int64, blockHash string, targets []int) {
	for _, voter := range voters {
		vote := finalityVoteFor(c.t, c.nodes[voter], c.ids[voter], height, blockHash)
		for _, target := range targets {
			c.nodes[target].handleFinalityVote(vote)
		}
	}
}

// extendChain produces count blocks within one partition, collecting the
// partition's own votes for each block (never enough for a 5/10 or 3/10 side
// to finalize, but enough for a 7/10 side).
func (c *partitionCluster) extendChain(members []int, count int, firstBlockTxs []any) []*ledger.Block {
	blocks := make([]*ledger.Block, 0, count)
	for index := 0; index < count; index++ {
		proposer, round := c.proposerIn(members)
		txs := []any{}
		if index == 0 && len(firstBlockTxs) > 0 {
			txs = firstBlockTxs
		}
		block := c.buildAndDeliver(members[0], proposer, txs, round, members)
		c.castVotes(members, block.Index, block.CurrentBlockHash, members)
		blocks = append(blocks, block)
	}
	return blocks
}

// deliverCanonical replays a source node's whole chain into the targets, in
// order, which is exactly what catch-up sync does.
func (c *partitionCluster) deliverCanonical(source int, targets []int) {
	node := c.nodes[source]
	node.mu.Lock()
	chain := append([]*ledger.Block{}, node.blockchain.Chain...)
	node.mu.Unlock()
	for _, block := range chain {
		if block.Index == 0 {
			continue
		}
		for _, target := range targets {
			c.accept(target, block)
		}
	}
}

func (c *partitionCluster) tipHash(index int) string {
	return c.nodes[index].BlockHashAt(c.nodes[index].Tip().Index)
}

func (c *partitionCluster) finalized(index int) (int64, string) {
	return c.nodes[index].FinalizedHeight(), c.nodes[index].FinalizedHash()
}

func (c *partitionCluster) balances(index int) map[string]int64 {
	node := c.nodes[index]
	node.mu.Lock()
	defer node.mu.Unlock()
	return copyIntMap(node.blockchain.SAN)
}

// nonceOf reads an account nonce under the node lock.
func (c *partitionCluster) nonceOf(index int, address string) int64 {
	node := c.nodes[index]
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.blockchain.Nonces[address]
}

func (c *partitionCluster) assertConverged(indexes []int, wantHash string) {
	c.t.Helper()
	for _, index := range indexes {
		node := c.nodes[index]
		tip := node.Tip()
		if tip.CurrentBlockHash != wantHash {
			c.t.Fatalf("node %d tip %s, want converged tip %s (height %d)",
				index, tip.CurrentBlockHash, wantHash, tip.Index)
		}
	}
}

func (c *partitionCluster) assertRootsEqual(indexes []int) {
	c.t.Helper()
	want := c.nodes[indexes[0]].CurrentStateRoot()
	for _, index := range indexes[1:] {
		if got := c.nodes[index].CurrentStateRoot(); got != want {
			c.t.Fatalf("node %d state root %s, want %s", index, got, want)
		}
	}
}

// ---------------------------------------------------------------------- #
// A: 5/5 split, no side can finalize, heal converges
// ---------------------------------------------------------------------- #

func TestPartitionA_FiveFiveCannotFinalizeThenHeals(t *testing.T) {
	cluster := newPartitionCluster(t, 10)
	majority := []int{0, 1, 2, 3, 4}
	minority := []int{5, 6, 7, 8, 9}

	receiver := c20Address()
	transfer := transferPayload(t, cluster.nodes[0], cluster.ids[0], 1, receiver, "7")
	cluster.extendChain(majority, 3, []any{transfer})
	cluster.extendChain(minority, 2, nil)

	// 5/10 and 5/10 are both below 2/3: no side may finalize.
	for index := 0; index < 10; index++ {
		if height, _ := cluster.finalized(index); height != 0 {
			t.Fatalf("node %d finalized height %d during a 50/50 split", index, height)
		}
	}
	majorityTip := cluster.tipHash(0)
	if majorityTip == cluster.tipHash(5) {
		t.Fatalf("partition did not diverge")
	}

	// Heal: the longer majority branch wins everywhere and 10/10 votes
	// finalize it.
	cluster.deliverCanonical(0, []int{1, 2, 3, 4, 5, 6, 7, 8, 9})
	cluster.castVotes([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		cluster.nodes[0].Tip().Index, majorityTip, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})

	all := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	cluster.assertConverged(all, majorityTip)
	cluster.assertRootsEqual(all)
	finalizedHeight := cluster.nodes[0].Tip().Index
	for _, index := range all {
		height, hash := cluster.finalized(index)
		if height != finalizedHeight || hash != majorityTip {
			t.Fatalf("node %d finality (%d,%s) does not match the healed tip", index, height, hash)
		}
	}

	// A vote for a different hash at the already-finalized height can never
	// move the checkpoint.
	node := cluster.nodes[5]
	node.handleFinalityVote(finalityVoteFor(t, node, cluster.ids[9], finalizedHeight, strings.Repeat("ff", 32)))
	if height, hash := cluster.finalized(5); height != finalizedHeight || hash != majorityTip {
		t.Fatalf("finalized history was rewritten: (%d,%s)", height, hash)
	}
}

// ---------------------------------------------------------------------- #
// B: 7/3 split, majority finalizes, minority adopts after heal
// ---------------------------------------------------------------------- #

func TestPartitionB_SevenThreeMajorityFinalizesAndMinorityAdopts(t *testing.T) {
	cluster := newPartitionCluster(t, 10)
	majority := []int{0, 1, 2, 3, 4, 5, 6}
	minority := []int{7, 8, 9}

	receiver := c20Address()
	transfer := transferPayload(t, cluster.nodes[0], cluster.ids[0], 1, receiver, "5")
	majorityBlocks := cluster.extendChain(majority, 3, []any{transfer})
	cluster.extendChain(minority, 2, nil)

	majorityTip := majorityBlocks[len(majorityBlocks)-1].CurrentBlockHash
	if height, hash := cluster.finalized(0); height != majorityBlocks[2].Index || hash != majorityTip {
		t.Fatalf("7/10 majority did not finalize its tip: (%d,%s)", height, hash)
	}
	for _, index := range minority {
		if height, _ := cluster.finalized(index); height != 0 {
			t.Fatalf("minority node %d finalized height %d", index, height)
		}
	}

	// Heal: the minority adopts the majority chain.
	cluster.deliverCanonical(0, minority)
	all := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	cluster.assertConverged(all, majorityTip)
	cluster.assertRootsEqual(all)

	// The transfer executed exactly once: the receiver holds it and the
	// sender nonce advanced once past the deposit (nonce 0) and transfer
	// (nonce 1).
	receiverUnits := int64(5) * ledger.SANBase
	for _, index := range all {
		balances := cluster.balances(index)
		if balances[receiver] != receiverUnits {
			t.Fatalf("node %d receiver balance %d, want %d", index, balances[receiver], receiverUnits)
		}
	}
	if got := cluster.nonceOf(5, cluster.addrs[0]); got != 2 {
		t.Fatalf("sender nonce: got %d, want 2 (duplicate execution?)", got)
	}
}

// ---------------------------------------------------------------------- #
// C: isolated proposer, fallback within MaxProposerRounds
// ---------------------------------------------------------------------- #

func TestPartitionC_IsolatedProposerFallbackThenRejoins(t *testing.T) {
	cluster := newPartitionCluster(t, 7)
	all := []int{0, 1, 2, 3, 4, 5, 6}
	height := cluster.nodes[0].Tip().Index + 1
	expected := cluster.nodes[0].ExpectedProposer(height, 0)
	if expected == "" {
		t.Fatalf("no expected proposer")
	}
	isolated := -1
	majority := []int{}
	for index, address := range cluster.addrs {
		if address == expected {
			isolated = index
			continue
		}
		majority = append(majority, index)
	}
	if isolated == -1 {
		t.Fatalf("expected proposer not found in the validator set")
	}

	proposer, round := cluster.proposerIn(majority)
	if round <= 0 {
		t.Fatalf("fallback round did not advance: %d", round)
	}
	maxRounds := int64(cluster.nodes[0].config.MaxProposerRounds)
	if round >= maxRounds {
		t.Fatalf("fallback round %d exceeds max_proposer_rounds %d", round, maxRounds)
	}
	block := cluster.buildAndDeliver(majority[0], proposer, nil, round, majority)
	for _, index := range majority {
		if cluster.tipHash(index) != block.CurrentBlockHash {
			t.Fatalf("majority node %d did not accept the fallback block", index)
		}
	}
	beforeRejoin := cluster.tipHash(isolated)
	if beforeRejoin == block.CurrentBlockHash {
		t.Fatalf("isolated proposer somehow followed a block it never received")
	}

	// When the isolated proposer returns it catches up and follows the
	// majority chain.
	cluster.deliverCanonical(majority[0], []int{isolated})
	if got := cluster.tipHash(isolated); got != block.CurrentBlockHash {
		t.Fatalf("returning proposer tip %s, want majority %s", got, block.CurrentBlockHash)
	}
	cluster.assertConverged(all, block.CurrentBlockHash)
}

// ---------------------------------------------------------------------- #
// D: full isolation, invalid branch, mempool recovery
// ---------------------------------------------------------------------- #

func TestPartitionD_FullIsolationHealsWithMempoolRecovery(t *testing.T) {
	cluster := newPartitionCluster(t, 4)
	all := []int{0, 1, 2, 3}

	// Finalize a common prefix before the split so a rewrite is detectable.
	commonProposer, commonRound := cluster.proposerIn(all)
	common := cluster.buildAndDeliver(0, commonProposer, nil, commonRound, all)
	cluster.castVotes(all, common.Index, common.CurrentBlockHash, all)
	for _, index := range all {
		if height, hash := cluster.finalized(index); height != common.Index || hash != common.CurrentBlockHash {
			t.Fatalf("common prefix did not finalize on node %d", index)
		}
	}

	// Node 1 includes a transfer in its isolated branch so the reorg can
	// requeue it; node 0 builds the longest branch (2 blocks), others 1.
	receiver := c20Address()
	transfer := transferPayload(t, cluster.nodes[1], cluster.ids[1], 1, receiver, "9")
	cluster.extendChain([]int{1}, 1, []any{transfer})
	cluster.extendChain([]int{2}, 1, nil)
	cluster.extendChain([]int{3}, 1, nil)
	node0Blocks := cluster.extendChain([]int{0}, 2, nil)
	node0Tip := node0Blocks[1].CurrentBlockHash

	for _, index := range all {
		if height, hash := cluster.finalized(index); height != common.Index || hash != common.CurrentBlockHash {
			t.Fatalf("node %d rewrote finalized history during isolation", index)
		}
	}

	// Heal every node onto node 0's longer branch. Node 1's branch is
	// abandoned and its transfer must re-enter the mempool (not be lost, not
	// be executed twice).
	cluster.deliverCanonical(0, []int{1, 2, 3})
	cluster.assertConverged(all, node0Tip)
	if got := cluster.nodes[1].PendingCount(); got != 1 {
		t.Fatalf("abandoned branch transaction did not re-enter the mempool: %d pending", got)
	}

	// Votes finalize the healed tip without touching the earlier checkpoint.
	cluster.castVotes(all, cluster.nodes[0].Tip().Index, node0Tip, all)
	for _, index := range all {
		height, hash := cluster.finalized(index)
		if height != cluster.nodes[0].Tip().Index || hash != node0Tip {
			t.Fatalf("node %d finality after heal: (%d,%s)", index, height, hash)
		}
	}

	// The requeued transfer commits exactly once, then a replay is rejected.
	cluster.mineRequeued(all, transfer)

	// An invalid branch is discarded and never changes the tip.
	invalid := buildProposal(t, cluster.nodes[0], cluster.ids[0], nil, 0)
	invalid.StateRoot = strings.Repeat("00", 32)
	invalid.CurrentBlockHash = invalid.CalculateHash()
	invalid.ValidatorSignature = cluster.ids[0].SignHex([]byte(invalid.CurrentBlockHash))
	before := cluster.tipHash(0)
	if cluster.accept(0, invalid) {
		t.Fatalf("invalid state-root block was accepted")
	}
	if after := cluster.tipHash(0); after != before {
		t.Fatalf("invalid block changed the tip: %s -> %s", before, after)
	}
}

// mineRequeued commits the pooled transfer and checks replay protection.
func (c *partitionCluster) mineRequeued(targets []int, transfer map[string]any) {
	c.t.Helper()
	beforeNonce := c.nonceOf(0, c.addrs[1])
	if c.nodes[1].PendingCount() != 1 {
		c.t.Fatalf("expected exactly one pooled transaction")
	}
	proposer, round := c.proposerIn(targets)
	c.buildAndDeliver(0, proposer, []any{transfer}, round, targets)
	if got := c.nonceOf(0, c.addrs[1]); got != beforeNonce+1 {
		c.t.Fatalf("requeued transaction did not execute once: nonce %d", got)
	}
	if c.nodes[1].PendingCount() != 0 {
		c.t.Fatalf("committed transaction stayed in the mempool")
	}
	if c.nodes[1].IngestTransaction(transfer) {
		c.t.Fatalf("replayed transaction was accepted into the mempool")
	}
}

// c20Address returns a deterministic receiver address.
func c20Address() string {
	return "0x" + strings.Repeat("ce", 20)
}
