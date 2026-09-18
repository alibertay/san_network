package netnode

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// extendChain builds a block once and commits exactly that block, so branch
// blocks fed to another node form a hash-linked chain.
func extendChain(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any) *ledger.Block {
	t.Helper()
	block := buildProposal(t, node, identity, txs, -1)
	commitStaged(t, node, block)
	return block
}

// extendChainAt builds a fork block with an explicit timestamp so it cannot
// collide with an already canonical block at the same height.
func extendChainAt(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any, timestamp float64) *ledger.Block {
	t.Helper()
	round := proposalRoundFor(node, memoizedIdentityAddress(identity))
	minimum := node.blockchain.Tip().TimestampFloat() + float64(round)*node.blockchain.ProposerTimeout()*0.8
	if minimum >= timestamp {
		timestamp = minimum + 0.001
	}
	block := buildProposalAt(t, node, identity, txs, round, timestamp)
	commitStaged(t, node, block)
	return block
}

// ---------------------------------------------------------------------- #
// Deterministic orphan iteration and tie-breaking
// ---------------------------------------------------------------------- #

// TestOrphanTieBreakDeterministic pins the fork-choice result when two equal
// length branches compete: the lexicographically smaller tip hash wins, on
// every run.
func TestOrphanTieBreakDeterministic(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // canonical height 2
	commitProposal(t, node, vs.ids[0], nil) // canonical height 3

	tip2 := node.blockchain.Chain[2]
	base := node.blockchain.Tip().TimestampFloat()
	first := ledger.NewBlock(3, tip2.CurrentBlockHash, vs.ids[0].PublicKeyHex(), nil,
		[]any{}, base+0.10, node.chainID, nil, 0, nil)
	first.ValidatorSignature = vs.ids[0].SignHex([]byte(first.CurrentBlockHash))
	second := ledger.NewBlock(3, tip2.CurrentBlockHash, vs.ids[0].PublicKeyHex(), nil,
		[]any{}, base+0.20, node.chainID, nil, 0, nil)
	second.ValidatorSignature = vs.ids[0].SignHex([]byte(second.CurrentBlockHash))

	node.mu.Lock()
	node.bufferOrphan(first)
	node.bufferOrphan(second)
	baseline := ""
	for run := 0; run < 100; run++ {
		candidate := node.bestOrphanChain()
		if candidate == nil {
			node.mu.Unlock()
			t.Fatalf("no candidate chain found")
		}
		got := candidate[len(candidate)-1].CurrentBlockHash
		if baseline == "" {
			baseline = got
			want := first.CurrentBlockHash
			if second.CurrentBlockHash < first.CurrentBlockHash {
				want = second.CurrentBlockHash
			}
			if got != want {
				node.mu.Unlock()
				t.Fatalf("tie-break picked %s, want the smaller hash %s", got, want)
			}
		}
		if got != baseline {
			node.mu.Unlock()
			t.Fatalf("bestOrphanChain changed between runs: %s -> %s", baseline, got)
		}
	}
	node.mu.Unlock()
}

// TestBufferOrphanEvictionDeterministic checks that overflow eviction removes
// the same (lowest index, then lowest hash) entry regardless of insertion
// order.
func TestBufferOrphanEvictionDeterministic(t *testing.T) {
	makeOrphan := func(index int64, hash string) *ledger.Block {
		return &ledger.Block{Index: index, CurrentBlockHash: hash, PreviousBlockHash: "parent"}
	}
	blocks := []*ledger.Block{
		makeOrphan(10, strings.Repeat("cc", 32)),
		makeOrphan(10, strings.Repeat("aa", 32)),
		makeOrphan(10, strings.Repeat("bb", 32)),
	}
	orders := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {2, 0, 1}}
	baseline := map[string]bool{}
	for run, order := range orders {
		node, err := NewNode(consensusConfig(), mustIdentity(t))
		if err != nil {
			t.Fatalf("NewNode: %v", err)
		}
		node.config.MaxOrphans = 2
		for _, index := range order {
			node.bufferOrphan(blocks[index])
		}
		kept := map[string]bool{}
		for hash := range node.orphans {
			kept[hash] = true
		}
		if len(kept) != 2 {
			t.Fatalf("orphan buffer size: got %d, want 2", len(kept))
		}
		if run == 0 {
			baseline = kept
			continue
		}
		if fmt.Sprint(kept) != fmt.Sprint(baseline) {
			t.Fatalf("eviction depends on insertion order: %v vs %v", kept, baseline)
		}
	}
	if baseline[strings.Repeat("aa", 32)] {
		t.Fatalf("eviction did not drop the lowest hash first: %v", baseline)
	}
}

// ---------------------------------------------------------------------- #
// Longest-chain, finality floor, invalid forks
// ---------------------------------------------------------------------- #

// TestLongestVerifiedBranchWinsAndFinalityHolds builds a competing branch that
// preserves finalized history (reorg allowed) and one that rewrites it
// (rejected).
func TestLongestVerifiedBranchWinsAndFinalityHolds(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	block2 := commitProposal(t, node, vs.ids[0], nil)
	commitProposal(t, node, vs.ids[0], nil) // height 3

	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block2.Index, block2.CurrentBlockHash))
	if node.FinalizedHeight() != block2.Index {
		t.Fatalf("setup did not finalize height %d", block2.Index)
	}

	// Branch A keeps the finalized block and extends to height 5.
	branchA := branchNode(t, vs, vs.ids[0], block2.Index)
	blocksA := []*ledger.Block{
		extendChainAt(t, branchA, vs.ids[0], nil, branchA.blockchain.Tip().TimestampFloat()+5.0),
	}
	for i := 0; i < 2; i++ {
		blocksA = append(blocksA, extendChain(t, branchA, vs.ids[0], nil))
	}
	longestA := blocksA[len(blocksA)-1]
	for i := len(blocksA) - 1; i >= 0; i-- {
		if !ingestBlock(t, node, blocksA[i]) {
			t.Fatalf("branch A block %d was rejected", blocksA[i].Index)
		}
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != longestA.CurrentBlockHash {
		t.Fatalf("longest valid branch did not win: got %s, want %s", got, longestA.CurrentBlockHash)
	}
	if node.CurrentStateRoot() != stringValue(longestA.StateRoot) {
		t.Fatalf("state root after reorg mismatch")
	}

	// Branch B forks below the finalized checkpoint and is longer; it must be
	// rejected without touching the tip.
	branchB := branchNode(t, vs, vs.ids[0], block2.Index-1)
	blocksB := []*ledger.Block{
		extendChainAt(t, branchB, vs.ids[0], nil, branchB.blockchain.Tip().TimestampFloat()+6.0),
	}
	for i := int64(0); i < 4; i++ {
		blocksB = append(blocksB, extendChain(t, branchB, vs.ids[0], nil))
	}
	beforeTip := node.blockchain.Tip().CurrentBlockHash
	beforeReorgs := metricValue(node, "reorgs")
	for i := 0; i < len(blocksB); i++ {
		ingestBlock(t, node, blocksB[i])
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != beforeTip {
		t.Fatalf("a branch rewriting finalized history was accepted: %s", got)
	}
	if got := metricValue(node, "reorgs"); got != beforeReorgs {
		t.Fatalf("reorg counter moved for a rejected branch")
	}
}

// TestInvalidForkIsDiscarded tampers a branch block's state root and asserts
// the reorg is rolled back completely.
func TestInvalidForkIsDiscarded(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // height 2
	stateRootBefore := node.CurrentStateRoot()
	tipBefore := node.blockchain.Tip().CurrentBlockHash

	branch := branchNode(t, vs, vs.ids[0], 1)
	bad := buildProposal(t, branch, vs.ids[0], nil, -1)
	bad.StateRoot = strings.Repeat("00", 32)
	bad.CurrentBlockHash = bad.CalculateHash()
	bad.ValidatorSignature = vs.ids[0].SignHex([]byte(bad.CurrentBlockHash))
	branch.blockchain.Chain = append(branch.blockchain.Chain, bad)
	branch.chainHashes[bad.CurrentBlockHash] = struct{}{}
	extension := buildProposal(t, branch, vs.ids[0], nil, -1)

	if !ingestBlock(t, node, bad) {
		t.Fatalf("cheap fork verification should have buffered the tampered block")
	}
	ingestBlock(t, node, extension)

	if node.blockchain.Tip().CurrentBlockHash != tipBefore {
		t.Fatalf("tip changed after an invalid branch replay")
	}
	if node.CurrentStateRoot() != stateRootBefore {
		t.Fatalf("state was not restored after a failed replay")
	}
	if metricValue(node, "reorgs") != 0 {
		t.Fatalf("failed replay counted as a reorg")
	}
}

// TestTamperedForkTransactionRejectedAtBufferTime covers the cheap fork check.
func TestTamperedForkTransactionRejectedAtBufferTime(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil)

	transfer := transferPayload(t, node, vs.ids[0], 1, "0x"+strings.Repeat("77", 20), "1")
	tampered := deepCopyStringMap(transfer)
	tampered["value"] = "999"
	branch := branchNode(t, vs, vs.ids[0], 1)
	block := ledger.NewBlock(2, branch.blockchain.Tip().CurrentBlockHash, vs.ids[0].PublicKeyHex(), nil,
		[]any{tampered}, nowSeconds(), node.chainID, nil, 0, nil)
	block.ValidatorSignature = vs.ids[0].SignHex([]byte(block.CurrentBlockHash))

	if ingestBlock(t, node, block) {
		t.Fatalf("fork block with a tampered transaction was buffered")
	}
	if len(node.orphans) != 0 {
		t.Fatalf("tampered fork block stayed in the orphan buffer")
	}
}

// TestOrphanDepthBoundsRejected checks the max_reorg_depth / max_orphans gates.
func TestOrphanDepthBoundsRejected(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	node.config.MaxReorgDepth = 2
	node.config.MaxOrphans = 3
	for i := 0; i < 5; i++ {
		commitProposal(t, node, vs.ids[0], nil)
	}
	tooOld := ledger.NewBlock(node.blockchain.Tip().Index-2, strings.Repeat("01", 32),
		vs.ids[0].PublicKeyHex(), nil, []any{}, nowSeconds(), node.chainID, nil, 0, nil)
	tooOld.ValidatorSignature = vs.ids[0].SignHex([]byte(tooOld.CurrentBlockHash))
	if ingestBlock(t, node, tooOld) {
		t.Fatalf("orphan beyond max_reorg_depth was accepted")
	}
	tooNew := ledger.NewBlock(node.blockchain.Tip().Index+4, strings.Repeat("02", 32),
		vs.ids[0].PublicKeyHex(), nil, []any{}, nowSeconds(), node.chainID, nil, 0, nil)
	tooNew.ValidatorSignature = vs.ids[0].SignHex([]byte(tooNew.CurrentBlockHash))
	if ingestBlock(t, node, tooNew) {
		t.Fatalf("orphan beyond max_orphans was accepted")
	}
}

// TestDelayedParentConnects feeds a child before its parent, then the parent,
// and expects the branch to assemble.
func TestDelayedParentConnects(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // height 2

	branch := branchNode(t, vs, vs.ids[0], 1)
	// Give the fork block a distinct timestamp so it cannot coincide with the
	// canonical block at the same height (identical content would hash equal).
	first := buildProposalAt(t, branch, vs.ids[0], nil, 0,
		node.blockchain.Tip().TimestampFloat()+1.0)
	commitStaged(t, branch, first)
	second := buildProposal(t, branch, vs.ids[0], nil, -1)

	if !ingestBlock(t, node, second) {
		t.Fatalf("child with an unknown parent was not buffered")
	}
	if node.blockchain.Tip().CurrentBlockHash == second.CurrentBlockHash {
		t.Fatalf("child connected before its parent arrived")
	}
	if !ingestBlock(t, node, first) {
		t.Fatalf("parent block was rejected")
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != second.CurrentBlockHash {
		// After both blocks the branch should be long enough to reorg.
		if node.blockchain.Tip().Index != second.Index {
			t.Fatalf("delayed branch did not assemble: tip %s (height %d)",
				got, node.blockchain.Tip().Index)
		}
	}
}

// ---------------------------------------------------------------------- #
// Reorg mempool restoration
// ---------------------------------------------------------------------- #

// TestReorgRequeuesOnlyStillValidTransactions puts two transactions in the
// abandoned block and proves the replayed one is not restored while the still
// valid one is.
func TestReorgRequeuesOnlyStillValidTransactions(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	receiver := "0x" + strings.Repeat("55", 20)

	first := transferPayload(t, node, vs.ids[0], 1, receiver, "1")
	second := transferPayload(t, node, vs.ids[0], 2, receiver, "2")
	commitProposal(t, node, vs.ids[0], []any{first, second}) // height 2

	branch := branchNode(t, vs, vs.ids[0], 1)
	// Branch height 2 replays the first transfer, then extends to height 3.
	firstCopy := signTransaction(t, branch, vs.ids[0], map[string]any{
		"chain_id": node.chainID,
		"sender":   vs.ids[0].PublicKeyHex(),
		"nonce":    int64(1),
		"receiver": receiver,
		"value":    "1",
	}).Payload
	branchTwo := buildProposal(t, branch, vs.ids[0], []any{firstCopy}, -1)
	commitStaged(t, branch, branchTwo)
	branchThree := buildProposal(t, branch, vs.ids[0], nil, -1)

	if !ingestBlock(t, node, branchTwo) || !ingestBlock(t, node, branchThree) {
		t.Fatalf("branch blocks were rejected")
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != branchThree.CurrentBlockHash {
		t.Fatalf("reorg did not happen: tip %s, want %s", got, branchThree.CurrentBlockHash)
	}

	pool := node.PendingTxIDs()
	if len(pool) != 1 {
		t.Fatalf("mempool after reorg: got %d transactions (%v), want 1", len(pool), pool)
	}
	secondID := ledger.TxID(second)
	if pool[0] != secondID {
		t.Fatalf("requeued the wrong transaction: got %s, want %s", pool[0], secondID)
	}
	if node.blockchain.Nonces[vs.addrs[0]] != 2 {
		t.Fatalf("replayed nonce not applied on the new branch")
	}
}

// TestReorgRequeuesAllStillValidTransactions keeps both txs valid on the new
// branch and expects both in the pool in nonce order.
func TestReorgRequeuesAllStillValidTransactions(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	receiver := "0x" + strings.Repeat("66", 20)

	first := transferPayload(t, node, vs.ids[0], 1, receiver, "1")
	second := transferPayload(t, node, vs.ids[0], 2, receiver, "2")
	commitProposal(t, node, vs.ids[0], []any{first, second})

	branch := branchNode(t, vs, vs.ids[0], 1)
	branchTwo := extendChain(t, branch, vs.ids[0], nil)
	branchThree := buildProposal(t, branch, vs.ids[0], nil, -1)
	if !ingestBlock(t, node, branchTwo) || !ingestBlock(t, node, branchThree) {
		t.Fatalf("branch blocks were rejected")
	}

	pool := node.PendingTxIDs()
	if len(pool) != 2 {
		t.Fatalf("mempool after reorg: got %d transactions (%v), want 2", len(pool), pool)
	}
	if pool[0] != ledger.TxID(first) || pool[1] != ledger.TxID(second) {
		t.Fatalf("requeued transactions are out of nonce order: %v", pool)
	}
}

// TestReorgRebuildsReceiptsTxIndexAndSnapshots checks the persisted indexes and
// snapshot pruning on a store-backed reorg.
func TestReorgRebuildsReceiptsTxIndexAndSnapshots(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	vs := newValidatorSetWithKV(t, 1, kv)
	node := vs.node
	receiverOld := "0x" + strings.Repeat("77", 20)
	receiverNew := "0x" + strings.Repeat("88", 20)

	oldTx := transferPayload(t, node, vs.ids[0], 1, receiverOld, "1")
	oldBlock := commitProposal(t, node, vs.ids[0], []any{oldTx}) // height 2
	if err := node.store.SaveSnapshot(oldBlock.Index, oldBlock.CurrentBlockHash,
		node.blockchain.StateSnapshot(), node.storage.ToDict()); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	branch := branchNode(t, vs, vs.ids[0], 1)
	newTx := signTransaction(t, branch, vs.ids[0], map[string]any{
		"chain_id": node.chainID,
		"sender":   vs.ids[0].PublicKeyHex(),
		"nonce":    int64(1),
		"receiver": receiverNew,
		"value":    "1",
	}).Payload
	branchTwo := buildProposal(t, branch, vs.ids[0], []any{newTx}, -1)
	commitStaged(t, branch, branchTwo)
	branchThree := buildProposal(t, branch, vs.ids[0], nil, -1)
	if !ingestBlock(t, node, branchTwo) || !ingestBlock(t, node, branchThree) {
		t.Fatalf("branch blocks were rejected")
	}

	newTxID := ledger.TxID(newTx)
	record, present, err := node.store.TxLookup(newTxID)
	if err != nil || !present {
		t.Fatalf("tx index missing the new branch transaction: present=%v err=%v", present, err)
	}
	if stringValue(record["block_hash"]) != branchTwo.CurrentBlockHash {
		t.Fatalf("tx index points at the wrong block: %v", record)
	}
	if _, present, _ := node.store.TxLookup(ledger.TxID(oldTx)); present {
		t.Fatalf("tx index kept the reorged-out transaction")
	}
	receipts, present, err := node.store.ReceiptsForBlock(branchTwo.Index)
	if err != nil || !present || len(receipts) != 1 {
		t.Fatalf("receipts were not rebuilt for the new branch: %v %v %v", receipts, present, err)
	}
	if node.GetTransaction(newTxID) == nil {
		t.Fatalf("GetTransaction cannot resolve the new branch transaction")
	}
	if node.GetTransaction(ledger.TxID(oldTx)) != nil {
		t.Fatalf("GetTransaction still resolves the reorged-out transaction")
	}
	snapshots, err := node.store.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	for _, snapshot := range snapshots {
		if stringValue(snapshot["hash"]) == oldBlock.CurrentBlockHash {
			t.Fatalf("snapshot of the abandoned chain was kept")
		}
	}
	if node.CurrentStateRoot() != stringValue(branchThree.StateRoot) {
		t.Fatalf("state root after reorg mismatch")
	}
}

// TestDeepOrphanBranchAssembles feeds a 30-block branch tip-first and expects
// the node to buffer the chain and reorg once the fork block arrives.
func TestDeepOrphanBranchAssembles(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // height 2

	branch := branchNode(t, vs, vs.ids[0], 1)
	// The fork block must differ from the canonical block at the same height.
	first := buildProposalAt(t, branch, vs.ids[0], nil, 0, node.blockchain.Tip().TimestampFloat()+1.0)
	commitStaged(t, branch, first)
	blocks := []*ledger.Block{first}
	for i := 0; i < 29; i++ {
		blocks = append(blocks, extendChain(t, branch, vs.ids[0], nil))
	}
	tip := blocks[len(blocks)-1]

	for i := len(blocks) - 1; i >= 0; i-- {
		if !ingestBlock(t, node, blocks[i]) {
			t.Fatalf("deep orphan block %d was rejected", blocks[i].Index)
		}
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != tip.CurrentBlockHash {
		t.Fatalf("deep orphan branch did not assemble: tip %s, want %s", got, tip.CurrentBlockHash)
	}
}

// TestReorgRetainsAbandonedBranchAncestors proves that a reorg keeps the
// replaced blocks as orphans, so a later, longer branch descending from them
// can still win without waiting for a re-fetch.
func TestReorgRetainsAbandonedBranchAncestors(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // height 2
	commitProposal(t, node, vs.ids[0], nil) // height 3
	abandoned := node.blockchain.Chain[2]   // height 2, replaced by branch A

	branchA := branchNode(t, vs, vs.ids[0], 1)
	blocksA := []*ledger.Block{
		extendChainAt(t, branchA, vs.ids[0], nil, branchA.blockchain.Tip().TimestampFloat()+5.0),
	}
	for i := 0; i < 2; i++ {
		blocksA = append(blocksA, extendChain(t, branchA, vs.ids[0], nil))
	}
	for _, block := range blocksA {
		ingestBlock(t, node, block)
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != blocksA[2].CurrentBlockHash {
		t.Fatalf("branch A did not reorg")
	}
	if node.blockchain.Tip().Index != 4 {
		t.Fatalf("branch A tip: got %d, want 4", node.blockchain.Tip().Index)
	}
	if _, present := node.orphans[abandoned.CurrentBlockHash]; !present {
		t.Fatalf("abandoned canonical block was not retained as an orphan")
	}

	// Branch B forks from the abandoned block and is longer; it must win.
	branchB := branchNode(t, vs, vs.ids[0], 2)
	blocksB := []*ledger.Block{
		extendChainAt(t, branchB, vs.ids[0], nil, branchB.blockchain.Tip().TimestampFloat()+6.0),
	}
	for i := 0; i < 2; i++ {
		blocksB = append(blocksB, extendChain(t, branchB, vs.ids[0], nil))
	}
	for _, block := range blocksB {
		ingestBlock(t, node, block)
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != blocksB[2].CurrentBlockHash {
		t.Fatalf("longer branch through the retained ancestor did not win: tip %s", got)
	}
}

// ---------------------------------------------------------------------- #
// Seeded randomized fork torture
// ---------------------------------------------------------------------- #

// TestForkChoiceSeededRandomBranches is a deterministic property test: a
// seeded scenario builds competing valid branches (random fork height, random
// lengths, shuffled delivery order) once, then replays the identical inputs on
// two fresh nodes and expects both to converge on the longest valid tip.
func TestForkChoiceSeededRandomBranches(t *testing.T) {
	ids := []*ledger.NodeIdentity{mustIdentity(t)}
	for seed := int64(1); seed <= 6; seed++ {
		scenario := buildForkScenario(t, ids, seed)
		first := replayForkScenario(t, ids, scenario)
		second := replayForkScenario(t, ids, scenario)
		if first != second {
			t.Fatalf("seed %d is not deterministic: %s vs %s", seed, first, second)
		}
		if first != scenario.wantTip {
			t.Fatalf("seed %d: tip %s, want the longest branch tip %s", seed, first, scenario.wantTip)
		}
	}
}

type forkScenario struct {
	canonical []*ledger.Block
	branch    []*ledger.Block
	order     []int
	wantTip   string
}

func buildForkScenario(t *testing.T, ids []*ledger.NodeIdentity, seed int64) forkScenario {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	vs := newValidatorSetFromIDs(t, ids, nil, nil)
	node := vs.node
	canonical := append([]*ledger.Block{}, node.blockchain.Chain...)    // genesis + height 1 (deposits)
	canonical = append(canonical, extendChain(t, node, vs.ids[0], nil)) // height 2
	canonical = append(canonical, extendChain(t, node, vs.ids[0], nil)) // height 3

	type branch struct {
		forkHeight int64
		blocks     []*ledger.Block
	}
	buildBranch := func(forkHeight, length int64, timestampOffset float64) branch {
		builder := branchNode(t, vs, vs.ids[0], forkHeight)
		blocks := make([]*ledger.Block, 0, length)
		for n := int64(0); n < length; n++ {
			// Mix in non-zero rounds; with a single validator the expected
			// proposer is unchanged and the timestamp rule justifies the round.
			round := int64(0)
			if rng.Intn(2) == 1 {
				round = 1 + rng.Int63n(2)
			}
			var block *ledger.Block
			if n == 0 {
				// A distinct timestamp guarantees the fork block differs from
				// the canonical block at the same height and from the other
				// branch's fork block.
				base := builder.blockchain.Tip().TimestampFloat()
				timestamp := base + timestampOffset
				required := base + float64(round)*builder.blockchain.ProposerTimeout()*0.8
				if required >= timestamp {
					timestamp = required + 0.001
				}
				block = buildProposalAt(t, builder, vs.ids[0], nil, round, timestamp)
			} else {
				block = buildProposal(t, builder, vs.ids[0], nil, round)
			}
			commitStaged(t, builder, block)
			blocks = append(blocks, block)
		}
		return branch{forkHeight: forkHeight, blocks: blocks}
	}
	// Both branches fork at the same height so their common ancestors stay on
	// the canonical chain and the longest-chain rule is fully testable.
	forkHeight := 1 + rng.Int63n(2) // fork at height 1 or 2
	length0 := int64(4-forkHeight) + 1 + rng.Int63n(2)
	length1 := int64(4-forkHeight) + 1 + rng.Int63n(2)
	if length0 == length1 {
		length1++
	}
	branches := []branch{buildBranch(forkHeight, length0, 5.0), buildBranch(forkHeight, length1, 6.0)}

	longest := branches[0]
	for _, candidate := range branches[1:] {
		if candidate.forkHeight+int64(len(candidate.blocks)) > longest.forkHeight+int64(len(longest.blocks)) {
			longest = candidate
		}
	}
	all := []*ledger.Block{}
	for _, item := range branches {
		all = append(all, item.blocks...)
	}
	order := make([]int, len(all))
	for i := range order {
		order[i] = i
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	return forkScenario{
		canonical: canonical,
		branch:    all,
		order:     order,
		wantTip:   longest.blocks[len(longest.blocks)-1].CurrentBlockHash,
	}
}

func replayForkScenario(t *testing.T, ids []*ledger.NodeIdentity, scenario forkScenario) string {
	t.Helper()
	config := validatorSetConfig(t, ids)
	node, err := NewNode(config, ids[0])
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	for _, block := range scenario.canonical {
		if block.Index == 0 {
			continue // fresh nodes already carry the same genesis block
		}
		commitStaged(t, node, block)
	}
	for _, index := range scenario.order {
		ingestBlock(t, node, scenario.branch[index])
	}
	if got := node.blockchain.Tip().CurrentBlockHash; got != scenario.wantTip {
		return got
	}
	if got := node.CurrentStateRoot(); got != stringValue(node.blockchain.Tip().StateRoot) {
		t.Fatalf("state root mismatch after torture")
	}
	return node.blockchain.Tip().CurrentBlockHash
}
