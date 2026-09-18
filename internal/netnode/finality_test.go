package netnode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

func metricValue(node *Node, name string) int64 {
	node.metricsMu.Lock()
	defer node.metricsMu.Unlock()
	return node.metrics[name]
}

func commitStaged(t *testing.T, node *Node, block *ledger.Block) {
	t.Helper()
	node.mu.Lock()
	ok := node.commitBlock(block, true)
	node.mu.Unlock()
	if !ok {
		t.Fatalf("commitBlock failed for height %d", block.Index)
	}
}

// TestVotesBeforeBlockAreTalliedOnCommit stages 2/3 votes for a block that has
// not arrived yet and checks the commit drains them into finality.
func TestVotesBeforeBlockAreTalliedOnCommit(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := buildProposal(t, node, vs.ids[0], nil, -1)

	for _, identity := range vs.ids[:2] {
		node.handleFinalityVote(finalityVoteFor(t, node, identity, block.Index, block.CurrentBlockHash))
	}
	if node.FinalizedHeight() != 0 {
		t.Fatalf("finality advanced before the block was committed")
	}
	commitStaged(t, node, block)

	if got := node.FinalizedHeight(); got != block.Index {
		t.Fatalf("finalized height: got %d, want %d", got, block.Index)
	}
	if got := node.FinalizedHash(); got != block.CurrentBlockHash {
		t.Fatalf("finalized hash: got %s, want %s", got, block.CurrentBlockHash)
	}
}

// TestVoteArbitraryOrderAndDuplicates feeds a shuffled, duplicated vote set
// and expects exactly one finalization.
func TestVoteArbitraryOrderAndDuplicates(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	order := []int{2, 0, 1, 0, 2, 1, 1}
	for _, index := range order {
		node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[index], block.Index, block.CurrentBlockHash))
	}
	if got := node.FinalizedHeight(); got != block.Index {
		t.Fatalf("finalized height: got %d, want %d", got, block.Index)
	}
	if got := metricValue(node, "votes_dropped_duplicate"); got == 0 {
		t.Fatalf("duplicate votes were not recorded as dropped duplicates")
	}
	if got := node.FinalizedHash(); got != block.CurrentBlockHash {
		t.Fatalf("finalized hash: got %s", got)
	}
}

// TestDuplicateVoteDoesNotDoubleCount pins that one validator cannot reach
// 2/3 alone by repeating its vote.
func TestDuplicateVoteDoesNotDoubleCount(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	vote := finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash)
	for i := 0; i < 5; i++ {
		node.handleFinalityVote(vote)
	}
	if node.FinalizedHeight() != 0 {
		t.Fatalf("a duplicated single-validator vote finalized the block")
	}
}

// TestConflictingVotesRecordEvidenceAndFinalizeOneHash covers equivocation.
func TestConflictingVotesRecordEvidenceAndFinalizeOneHash(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash))
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, strings.Repeat("cc", 32)))
	if len(node.EquivocationEvidence()) != 1 {
		t.Fatalf("expected 1 evidence record, got %d", len(node.EquivocationEvidence()))
	}
	if got := metricValue(node, "votes_dropped_equivocation"); got != 1 {
		t.Fatalf("equivocation drop metric: got %d, want 1", got)
	}

	for _, identity := range vs.ids[1:3] {
		node.handleFinalityVote(finalityVoteFor(t, node, identity, block.Index, block.CurrentBlockHash))
	}
	if node.FinalizedHeight() != block.Index || node.FinalizedHash() != block.CurrentBlockHash {
		t.Fatalf("canonical hash did not finalize after equivocation")
	}
}

// TestVotesForNonCanonicalHashDoNotFinalize covers votes naming a hash that is
// not the canonical block at that height.
func TestVotesForNonCanonicalHashDoNotFinalize(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)
	wrongHash := strings.Repeat("dd", 32)
	for _, identity := range vs.ids {
		node.handleFinalityVote(finalityVoteFor(t, node, identity, block.Index, wrongHash))
	}
	if node.FinalizedHeight() != 0 {
		t.Fatalf("finality advanced for a non-canonical hash")
	}
	heights := node.PendingFinalityHeights()
	if len(heights) != 1 || heights[0] != block.Index {
		t.Fatalf("pending finality heights: got %v, want [%d]", heights, block.Index)
	}
}

// TestVoteHeightsOutsideTheLookaheadAreDropped bounds staged vote spam.
func TestVoteHeightsOutsideTheLookaheadAreDropped(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	tip := node.blockchain.Tip().Index

	tooFar := finalityVoteFor(t, node, vs.ids[0], tip+VoteLookahead+1, strings.Repeat("ee", 32))
	node.handleFinalityVote(tooFar)
	// Height 0 has no frozen set and is at or below the tip: dropped.
	atOrBelowTip := finalityVoteFor(t, node, vs.ids[0], 0, node.blockchain.Chain[0].CurrentBlockHash)
	node.handleFinalityVote(atOrBelowTip)

	if len(node.PendingFinalityHeights()) != 0 {
		t.Fatalf("votes outside the lookahead were staged: %v", node.PendingFinalityHeights())
	}
	if got := metricValue(node, "votes_dropped_height"); got < 2 {
		t.Fatalf("height-drop metric: got %d, want >= 2", got)
	}
}

// TestValidatorSetChangeBetweenVoteAndTally freezes the weight set at commit.
func TestValidatorSetChangeBetweenVoteAndTally(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	// The set is frozen now; undelegate one validator in the next block.
	undelegate := validatorCommandPayload(t, node, vs.ids[1], 1, map[string]any{"command": "undelegate"})
	commitProposal(t, node, vs.ids[0], []any{undelegate})
	if _, active := node.blockchain.ActiveValidators()[vs.addrs[1]]; active {
		t.Fatalf("validator is still active after undelegate")
	}

	// Votes from the frozen set still count at the frozen weights.
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[2], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("frozen validator votes were not counted")
	}

	// A validator that joined after the height is not in the frozen set.
	late := mustIdentity(t)
	lateAddress := identityAddress(t, late)
	transfer := transferPayload(t, node, vs.ids[0], 1, lateAddress, "1200")
	deposit := validatorCommandPayload(t, node, late, 0, map[string]any{"command": "deposit", "amount": testStakeUnits})
	commitProposal(t, node, vs.ids[0], []any{transfer, deposit})
	before := metricValue(node, "votes_dropped_voter")
	node.handleFinalityVote(finalityVoteFor(t, node, late, block.Index, block.CurrentBlockHash))
	if got := metricValue(node, "votes_dropped_voter"); got != before+1 {
		t.Fatalf("late validator vote was not dropped (metric %d -> %d)", before, got)
	}
}

// TestSlashedValidatorVoteStillCountsForTheFrozenHeight covers a validator
// removed after voting.
func TestSlashedValidatorVoteStillCountsForTheFrozenHeight(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	// Validator 2 votes for the height, then is slashed in the next block.
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))

	height := node.blockchain.Tip().Index + 1
	voteA := finalityVoteFor(t, node, vs.ids[1], height, strings.Repeat("11", 32))
	voteB := finalityVoteFor(t, node, vs.ids[1], height, strings.Repeat("22", 32))
	evidence := validatorCommandPayload(t, node, vs.ids[0], 1, map[string]any{
		"command": "evidence", "vote_a": voteA, "vote_b": voteB,
	})
	commitProposal(t, node, vs.ids[0], []any{evidence})
	if _, present := node.blockchain.Validators[vs.addrs[1]]; present {
		t.Fatalf("validator was not slashed")
	}

	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[2], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("a pre-slash vote was dropped from the frozen tally")
	}
}

// TestFinalityCheckpointIsMonotonic asserts one hash per height and no
// regression.
func TestFinalityCheckpointIsMonotonic(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash))
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))

	// A competing hash at an already finalized height is ignored.
	node.mu.Lock()
	node.tallyFinality(block.Index, strings.Repeat("33", 32))
	node.mu.Unlock()
	if node.FinalizedHash() != block.CurrentBlockHash {
		t.Fatalf("finalized hash changed after a competing tally")
	}
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("finalized height regressed")
	}

	// A vote for a lower height cannot move the checkpoint back.
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index-1, strings.Repeat("44", 32)))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("finality regressed to a lower height")
	}
}

// TestFinalityStateSurvivesRestart persists the checkpoint and reloads it.
func TestFinalityStateSurvivesRestart(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	vs := newValidatorSetWithKV(t, 3, kv)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash))
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("setup did not finalize")
	}
	storedHash := node.FinalizedHash()

	reloaded, err := reloadNode(t, vs, kv)
	if err != nil {
		t.Fatalf("reload after finality: %v", err)
	}
	if got := reloaded.FinalizedHeight(); got != block.Index {
		t.Fatalf("restored finalized height: got %d, want %d", got, block.Index)
	}
	if got := reloaded.FinalizedHash(); got != storedHash {
		t.Fatalf("restored finalized hash: got %s, want %s", got, storedHash)
	}
	if got := reloaded.blockchain.Tip().Index; got != node.blockchain.Tip().Index {
		t.Fatalf("restored tip: got %d, want %d", got, node.blockchain.Tip().Index)
	}
}

// TestPersistedFinalityContradictionFailsStartup is the fatal-check test: a
// checkpoint that contradicts the canonical chain must stop the node.
func TestPersistedFinalityContradictionFailsStartup(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	vs := newValidatorSetWithKV(t, 3, kv)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash))
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("setup did not finalize")
	}

	t.Run("meta hash mismatch", func(t *testing.T) {
		corrupted := store.NewMemoryStore(":memory:")
		copyStore(t, kv, corrupted)
		if err := corrupted.Put([]byte("m:finalized_hash"), []byte(strings.Repeat("00", 32))); err != nil {
			t.Fatalf("corrupt meta: %v", err)
		}
		if _, err := reloadNode(t, vs, corrupted); err == nil {
			t.Fatalf("node started with a finality checkpoint that contradicts the canonical chain")
		} else if !strings.Contains(err.Error(), "finality") {
			t.Fatalf("startup error is not clear about finality: %v", err)
		}
	})

	t.Run("state checkpoint mismatch", func(t *testing.T) {
		corrupted := store.NewMemoryStore(":memory:")
		copyStore(t, kv, corrupted)
		if err := corrupted.Delete([]byte("m:finalized_height")); err != nil {
			t.Fatalf("delete meta: %v", err)
		}
		raw, _ := corrupted.Get([]byte("m:finality_state"))
		mutated := strings.Replace(string(raw),
			node.FinalizedHash(), strings.Repeat("ab", 32), 1)
		if err := corrupted.Put([]byte("m:finality_state"), []byte(mutated)); err != nil {
			t.Fatalf("corrupt finality state: %v", err)
		}
		if _, err := reloadNode(t, vs, corrupted); err == nil {
			t.Fatalf("node started with a contradictory finality_state checkpoint")
		}
	})
}

func copyStore(t *testing.T, from *store.MemoryStore, to *store.MemoryStore) {
	t.Helper()
	for key, value := range from.Snapshot() {
		if err := to.Put([]byte(key), value); err != nil {
			t.Fatalf("copy %q: %v", key, err)
		}
	}
}

// TestSyncingPastFinalizedBlocks pulls a finalized chain into a fresh node
// over the real Sync RPC.
func TestSyncingPastFinalizedBlocks(t *testing.T) {
	ports := freePorts(t, 8)
	vs := newValidatorSetCustom(t, 3, nil, func(config *NodeConfig) {
		config.APIPort = ports[0]
		config.PeerPort = ports[1]
		config.P2PPort = ports[2]
		config.ControllerPort = ports[3]
		config.WSTimeout = 1.0
	})
	source := vs.node
	for i := 0; i < 3; i++ {
		commitProposal(t, source, vs.ids[0], nil)
	}
	last := source.blockchain.Tip()
	source.handleFinalityVote(finalityVoteFor(t, source, vs.ids[0], last.Index, last.CurrentBlockHash))
	source.handleFinalityVote(finalityVoteFor(t, source, vs.ids[1], last.Index, last.CurrentBlockHash))
	if source.FinalizedHeight() != last.Index {
		t.Fatalf("source did not finalize its tip")
	}
	if err := source.Start(context.Background()); err != nil {
		t.Skipf("cannot bind source gRPC ports: %v", err)
	}
	defer source.Stop()

	config := consensusConfig()
	config.APIPort = ports[4]
	config.PeerPort = ports[5]
	config.P2PPort = ports[6]
	config.ControllerPort = ports[7]
	config.GenesisAllocations = vs.allocations
	config.WSTimeout = 1.0
	syncer, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode(syncer): %v", err)
	}
	if added := syncer.AddPeer(source.SelfPeerRecord()); added != 1 {
		t.Fatalf("AddPeer: got %d, want 1", added)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !syncer.Synchronize(ctx) {
		t.Fatalf("Synchronize reported no progress")
	}
	if got, want := syncer.blockchain.Tip().Index, last.Index; got != want {
		t.Fatalf("synced tip: got %d, want %d", got, want)
	}
	if got, want := syncer.CurrentStateRoot(), source.CurrentStateRoot(); got != want {
		t.Fatalf("synced state root: got %s, want %s", got, want)
	}
}

// TestPartitionOneThirdCannotFinalize models an isolated validator: finality
// only advances once the quorum heals.
func TestPartitionOneThirdCannotFinalize(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	block := commitProposal(t, node, vs.ids[0], nil)

	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != 0 {
		t.Fatalf("1/3 of the stake finalized a block")
	}
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[1], block.Index, block.CurrentBlockHash))
	if node.FinalizedHeight() != block.Index {
		t.Fatalf("finality did not heal after the partition")
	}
}
