package netnode

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// faultStore wraps a KeyValueStore and can reject batches or single puts,
// emulating a backend failure while never applying partial writes.
type faultStore struct {
	store.KeyValueStore
	mu          sync.Mutex
	failBatches bool
	failPuts    bool
}

func (s *faultStore) WriteBatch(batch *store.WriteBatch) error {
	s.mu.Lock()
	fail := s.failBatches
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("injected WriteBatch failure")
	}
	return s.KeyValueStore.WriteBatch(batch)
}

func (s *faultStore) Put(key, value []byte) error {
	s.mu.Lock()
	fail := s.failPuts
	s.mu.Unlock()
	if fail {
		return fmt.Errorf("injected Put failure")
	}
	return s.KeyValueStore.Put(key, value)
}

func (s *faultStore) armBatches(fail bool) {
	s.mu.Lock()
	s.failBatches = fail
	s.mu.Unlock()
}

func (s *faultStore) armPuts(fail bool) {
	s.mu.Lock()
	s.failPuts = fail
	s.mu.Unlock()
}

func faultMemoryStore() *faultStore {
	return &faultStore{KeyValueStore: store.NewMemoryStore(":memory:")}
}

// TestAppendBlockBatchFailureLeavesPreviousState injects a failure while
// committing block 2; the store must stay exactly at the block 1 state.
func TestAppendBlockBatchFailureLeavesPreviousState(t *testing.T) {
	kv := faultMemoryStore()
	vs := newValidatorSetWithKV(t, 1, kv)
	node := vs.node
	rootAfterBlock1 := node.CurrentStateRoot()
	receiver := "0x" + strings.Repeat("99", 20)

	kv.armBatches(true)
	transfer := transferPayload(t, node, vs.ids[0], 1, receiver, "1")
	commitProposal(t, node, vs.ids[0], []any{transfer})
	if node.blockchain.Tip().Index != 2 {
		t.Fatalf("in-memory commit did not happen")
	}
	if height, _ := node.store.HighestHeight(); height != 1 {
		t.Fatalf("store advanced to height %d despite the failed batch", height)
	}

	reloaded, err := reloadNode(t, vs, kv)
	if err != nil {
		t.Fatalf("reload after failed batch: %v", err)
	}
	if got := reloaded.blockchain.Tip().Index; got != 1 {
		t.Fatalf("restored tip: got %d, want 1", got)
	}
	if got := reloaded.CurrentStateRoot(); got != rootAfterBlock1 {
		t.Fatalf("restored state root differs from the last consistent state")
	}
	if reloaded.GetTransaction(ledger.TxID(transfer)) != nil {
		t.Fatalf("failed batch leaked a transaction index entry")
	}
	if receipts, present, _ := reloaded.store.ReceiptsForBlock(2); present || len(receipts) != 0 {
		t.Fatalf("failed batch leaked receipts for block 2")
	}
}

// TestReplaceChainBatchFailureKeepsOldChain injects a failure during a reorg
// write and asserts the old canonical chain survives in the store.
func TestReplaceChainBatchFailureKeepsOldChain(t *testing.T) {
	kv := faultMemoryStore()
	vs := newValidatorSetWithKV(t, 1, kv)
	node := vs.node
	commitProposal(t, node, vs.ids[0], nil) // height 2
	commitProposal(t, node, vs.ids[0], nil) // height 3
	tipBefore := node.blockchain.Tip().CurrentBlockHash
	rootBefore := node.CurrentStateRoot()

	branch := branchNode(t, vs, vs.ids[0], 1)
	var blocks []*ledger.Block
	for i := 0; i < 3; i++ {
		blocks = append(blocks, extendChain(t, branch, vs.ids[0], nil))
	}

	kv.armBatches(true)
	for _, block := range blocks {
		ingestBlock(t, node, block)
	}
	kv.armBatches(false)

	if got := node.blockchain.Tip().CurrentBlockHash; got != tipBefore {
		t.Fatalf("reorg was kept despite the failed replacement: %s", got)
	}
	if got := node.CurrentStateRoot(); got != rootBefore {
		t.Fatalf("in-memory state was not restored after the failed replacement")
	}
	if height, _ := node.store.HighestHeight(); height != 3 {
		t.Fatalf("store chain changed despite the failed replacement: height %d", height)
	}
	reloaded, err := reloadNode(t, vs, kv)
	if err != nil {
		t.Fatalf("reload after failed replacement: %v", err)
	}
	if got := reloaded.blockchain.Tip().CurrentBlockHash; got != tipBefore {
		t.Fatalf("reloaded chain does not match the old canonical chain")
	}
	if got := reloaded.CurrentStateRoot(); got != rootBefore {
		t.Fatalf("reloaded state root differs after the failed replacement")
	}
}

// TestFinalityPersistenceFailureKeepsCheckpoint injects a metadata failure and
// asserts the previously persisted checkpoint is still the one restored.
func TestFinalityPersistenceFailureKeepsCheckpoint(t *testing.T) {
	kv := faultMemoryStore()
	vs := newValidatorSetWithKV(t, 1, kv)
	node := vs.node
	block2 := commitProposal(t, node, vs.ids[0], nil)
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block2.Index, block2.CurrentBlockHash))
	if node.FinalizedHeight() != block2.Index {
		t.Fatalf("setup did not finalize height %d", block2.Index)
	}
	storedHeight, _ := kv.Get([]byte("m:finalized_height"))
	if string(storedHeight) != "2" {
		t.Fatalf("persisted checkpoint: got %q, want \"2\"", storedHeight)
	}

	kv.armPuts(true)
	block3 := commitProposal(t, node, vs.ids[0], nil)
	node.handleFinalityVote(finalityVoteFor(t, node, vs.ids[0], block3.Index, block3.CurrentBlockHash))
	if node.FinalizedHeight() != block3.Index {
		t.Fatalf("in-memory finality did not advance")
	}
	kv.armPuts(false)
	if got, _ := kv.Get([]byte("m:finalized_height")); string(got) != "2" {
		t.Fatalf("failed put changed the persisted checkpoint to %q", got)
	}

	reloaded, err := reloadNode(t, vs, kv)
	if err != nil {
		t.Fatalf("reload after failed finality write: %v", err)
	}
	if got := reloaded.FinalizedHeight(); got != block2.Index {
		t.Fatalf("restored checkpoint: got %d, want %d", got, block2.Index)
	}
	if got := reloaded.FinalizedHash(); got != block2.CurrentBlockHash {
		t.Fatalf("restored checkpoint hash: got %s, want %s", got, block2.CurrentBlockHash)
	}
}

// TestSnapshotWriteFailureKeepsPreviousSnapshot checks the snapshot batch.
func TestSnapshotWriteFailureKeepsPreviousSnapshot(t *testing.T) {
	kv := faultMemoryStore()
	vs := newValidatorSetWithKV(t, 1, kv)
	node := vs.node
	block2 := commitProposal(t, node, vs.ids[0], nil)
	if err := node.store.SaveSnapshot(block2.Index, block2.CurrentBlockHash,
		node.blockchain.StateSnapshot(), node.storage.ToDict()); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	block3 := commitProposal(t, node, vs.ids[0], nil)

	kv.armBatches(true)
	if err := node.store.SaveSnapshot(block3.Index, block3.CurrentBlockHash,
		node.blockchain.StateSnapshot(), node.storage.ToDict()); err == nil {
		t.Fatalf("SaveSnapshot succeeded while the batch layer was failing")
	}
	kv.armBatches(false)

	snapshots, err := node.store.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 1 || int64Value(snapshots[0]["height"]) != block2.Index {
		t.Fatalf("snapshot set changed after the failed write: %v", snapshots)
	}
}
