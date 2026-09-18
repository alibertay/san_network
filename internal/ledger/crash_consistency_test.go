package ledger

import (
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/alibertay/san_network/internal/ledger/store"
)

// interruptStore wraps a KeyValueStore and can reject batches/puts or apply
// only a prefix of a batch before failing. The prefix mode emulates a backend
// that is not actually atomic (LMDB is, but the chain code must never trust a
// partial write either way).
type interruptStore struct {
	store.KeyValueStore
	mu           sync.Mutex
	failBatch    bool
	failPut      bool
	applyPartial int // -1 disabled; otherwise number of ops applied before error
}

func (s *interruptStore) WriteBatch(batch *store.WriteBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failBatch {
		return fmt.Errorf("injected WriteBatch failure")
	}
	if s.applyPartial < 0 {
		return s.KeyValueStore.WriteBatch(batch)
	}
	ops := batch.ExportOps()
	applied := s.applyPartial
	if applied > len(ops) {
		applied = len(ops)
	}
	for index := 0; index < applied; index++ {
		op := ops[index]
		if op.Delete {
			_ = s.KeyValueStore.Delete(op.Key)
		} else {
			_ = s.KeyValueStore.Put(op.Key, op.Value)
		}
	}
	return fmt.Errorf("injected partial WriteBatch failure after %d op(s)", applied)
}

func (s *interruptStore) Put(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return fmt.Errorf("injected Put failure")
	}
	return s.KeyValueStore.Put(key, value)
}

func (s *interruptStore) armBatch(fail bool) {
	s.mu.Lock()
	s.failBatch = fail
	s.mu.Unlock()
}

func (s *interruptStore) armPut(fail bool) {
	s.mu.Lock()
	s.failPut = fail
	s.mu.Unlock()
}

func (s *interruptStore) setPartial(count int) {
	s.mu.Lock()
	s.applyPartial = count
	s.mu.Unlock()
}

func interruptMemoryStore() *interruptStore {
	return &interruptStore{KeyValueStore: store.NewMemoryStore(":memory:"), applyPartial: -1}
}

// newCrashStore builds a ChainStore over the wrapper.
func newCrashStore(t *testing.T, kv *interruptStore) *ChainStore {
	t.Helper()
	chainStore, err := NewChainStore(":memory:", "memory", kv)
	if err != nil {
		t.Fatalf("NewChainStore: %v", err)
	}
	return chainStore
}

func crashTestBlock(t *testing.T, index int64, previous string) (*Block, map[string]any, map[string]any) {
	t.Helper()
	block, state, storage := buildCrashBlock(index, previous)
	return block, state, storage
}

// buildCrashBlock returns a deterministic block plus its matching state and
// storage snapshots (no *testing.T so the process-kill child can use it).
func buildCrashBlock(index int64, previous string) (*Block, map[string]any, map[string]any) {
	balance := int64(1000 + index)
	state := map[string]any{
		"balances":      map[string]any{"0x" + "aa": balance},
		"nonces":        map[string]any{},
		"validators":    map[string]any{},
		"total_slashed": int64(0),
		"total_burned":  int64(0),
		"base_fee":      int64(1),
		"parameters":    map[string]any{"slash_bps": int64(5000)},
	}
	storage := map[string]any{"data": map[string]any{}, "functions": map[string]any{}, "contracts": map[string]any{}}
	root := StateRoot(StateInput{
		Balances:     map[string]int64{"0x" + "aa": balance},
		Nonces:       map[string]int64{},
		Validators:   map[string]map[string]any{},
		TotalSlashed: 0,
		Storage:      storage,
		Parameters:   map[string]int64{"slash_bps": 5000},
		BaseFee:      1,
		TotalBurned:  0,
	})
	block := NewBlock(index, previous, "validator", "signature", []any{},
		float64(index), "san-devnet-1", root, 0, nil)
	return block, state, storage
}

// stateInputFromSnapshot converts a persisted state/storage snapshot into the
// StateInput used to recompute the state root.
func stateInputFromSnapshot(state, storage map[string]any) StateInput {
	input := StateInput{
		Balances:   map[string]int64{},
		Nonces:     map[string]int64{},
		Validators: map[string]map[string]any{},
		Storage:    storage,
		Parameters: map[string]int64{},
	}
	if state == nil {
		return input
	}
	if raw, ok := state["balances"].(map[string]any); ok {
		for address, value := range raw {
			input.Balances[address] = toInt64(value)
		}
	}
	if raw, ok := state["nonces"].(map[string]any); ok {
		for address, value := range raw {
			input.Nonces[address] = toInt64(value)
		}
	}
	if raw, ok := state["validators"].(map[string]any); ok {
		for address, value := range raw {
			if record, ok := value.(map[string]any); ok {
				input.Validators[address] = record
			}
		}
	}
	if raw, ok := state["parameters"].(map[string]any); ok {
		for name, value := range raw {
			input.Parameters[name] = toInt64(value)
		}
	}
	input.TotalSlashed = toInt64(state["total_slashed"])
	input.TotalBurned = toInt64(state["total_burned"])
	input.BaseFee = toInt64(state["base_fee"])
	return input
}

// assertStateRootMatchesHead checks that the persisted state snapshot hashes to
// the head block's committed state root.
func assertStateRootMatchesHead(t *testing.T, chainStore *ChainStore) {
	t.Helper()
	highest, ok := chainStore.HighestHeight()
	if !ok {
		return
	}
	head, err := chainStore.LoadBlock(highest)
	if err != nil || head == nil || head.StateRoot == nil {
		return
	}
	state, err := chainStore.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state == nil {
		return
	}
	storage, err := chainStore.LoadStorage()
	if err != nil {
		t.Fatalf("LoadStorage: %v", err)
	}
	recomputed := StateRoot(stateInputFromSnapshot(state, storage))
	rootText, _ := head.StateRoot.(string)
	if recomputed != rootText {
		t.Fatalf("persisted state root %s does not match block %d state root %s",
			recomputed, highest, rootText)
	}
}

func appendCrashBlock(t *testing.T, chainStore *ChainStore, index int64, previous string) *Block {
	t.Helper()
	block, state, storage := crashTestBlock(t, index, previous)
	receipts := []any{map[string]any{"tx_id": fmt.Sprintf("tx-%d", index), "tx_index": int64(0)}}
	head := index
	if err := chainStore.AppendBlock(block, AppendOptions{
		Receipts: receipts, State: state, Storage: storage, Head: &head,
	}); err != nil {
		t.Fatalf("AppendBlock(%d): %v", index, err)
	}
	return block
}

// assertCanonicalChain fails when the canonical index, headers, bodies or head
// metadata disagree: the crash-consistency invariant for a reopened database.
func assertCanonicalChain(t *testing.T, chainStore *ChainStore) {
	t.Helper()
	highest, hasBlocks := chainStore.HighestHeight()
	headMeta, hasHead := chainStore.GetMeta("head")
	if !hasBlocks {
		if hasHead {
			t.Fatalf("head metadata %q exists without any canonical block", headMeta)
		}
		return
	}
	if hasHead && headMeta != strconv.FormatInt(highest, 10) {
		t.Fatalf("head metadata %q disagrees with highest height %d", headMeta, highest)
	}
	var previous string
	for height := int64(0); height <= highest; height++ {
		blockHash, ok := chainStore.BlockHashAt(height)
		if !ok {
			t.Fatalf("canonical height %d is missing its block hash", height)
		}
		block, err := chainStore.BlockByHash(blockHash)
		if err != nil {
			t.Fatalf("BlockByHash(%s): %v", blockHash, err)
		}
		if block == nil {
			t.Fatalf("canonical height %d points at a block with no header or body", height)
		}
		if block.Index != height {
			t.Fatalf("canonical height %d stores block index %d", height, block.Index)
		}
		if height > 0 && block.PreviousBlockHash != previous {
			t.Fatalf("block %d previous hash %s does not chain from %s", height, block.PreviousBlockHash, previous)
		}
		previous = blockHash
	}
}

func TestCrashDuringAppendBlockKeepsPreviousState(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")
	beforeState, err := chainStore.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	kv.armBatch(true)
	next, state, storage := crashTestBlock(t, 1, genesis.CurrentBlockHash)
	head := int64(1)
	err = chainStore.AppendBlock(next, AppendOptions{
		Receipts: []any{map[string]any{"tx_id": "tx-1", "tx_index": int64(0)}},
		State:    state, Storage: storage, Head: &head,
	})
	if err == nil {
		t.Fatalf("AppendBlock succeeded while the batch layer was failing")
	}
	kv.armBatch(false)

	if height, _ := chainStore.HighestHeight(); height != 0 {
		t.Fatalf("store advanced to height %d after the failed append", height)
	}
	if _, ok := chainStore.BlockHashAt(1); ok {
		t.Fatalf("failed append leaked a canonical index entry")
	}
	if receipts, present, _ := chainStore.ReceiptsForBlock(1); present || len(receipts) != 0 {
		t.Fatalf("failed append leaked receipts")
	}
	if _, present, _ := chainStore.TxLookup("tx-1"); present {
		t.Fatalf("failed append leaked a transaction index entry")
	}
	loaded, err := chainStore.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !reflect.DeepEqual(loaded, beforeState) {
		t.Fatalf("failed append changed the state snapshot")
	}
	assertCanonicalChain(t, chainStore)

	// A reopen over the same bytes must agree.
	reopened := newCrashStore(t, kv)
	if height, _ := reopened.HighestHeight(); height != 0 {
		t.Fatalf("reopened store height %d, want 0", height)
	}
	assertCanonicalChain(t, reopened)
}

// TestTornAppendBlockIsNotCanonical applies a prefix of the batch (header only)
// and asserts the partial write is not exposed as a canonical block and the
// chain metadata stays consistent.
func TestTornAppendBlockIsNotCanonical(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")

	kv.setPartial(1) // header written, canonical index/state/head not
	next, state, storage := crashTestBlock(t, 1, genesis.CurrentBlockHash)
	head := int64(1)
	if err := chainStore.AppendBlock(next, AppendOptions{State: state, Storage: storage, Head: &head}); err == nil {
		t.Fatalf("partial batch was reported as success")
	}
	kv.setPartial(-1)

	if height, _ := chainStore.HighestHeight(); height != 0 {
		t.Fatalf("torn write made block 1 canonical (height %d)", height)
	}
	if block, _ := chainStore.LoadBlock(1); block != nil {
		t.Fatalf("torn write exposed block 1 through the canonical loader")
	}
	if headMeta, _ := chainStore.GetMeta("head"); headMeta != "0" {
		t.Fatalf("torn write advanced head metadata to %q", headMeta)
	}
	assertCanonicalChain(t, chainStore)
}

func TestCrashDuringReplaceChainKeepsOldChain(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")
	block1 := appendCrashBlock(t, chainStore, 1, genesis.CurrentBlockHash)
	block2 := appendCrashBlock(t, chainStore, 2, block1.CurrentBlockHash)
	oldHead, _ := chainStore.GetMeta("head")

	replacement := []*Block{genesis, block1, block2}
	_, state, storage := crashTestBlock(t, 2, block1.CurrentBlockHash)

	kv.armBatch(true)
	err := chainStore.ReplaceChain(replacement, state, storage, nil)
	if err == nil {
		t.Fatalf("ReplaceChain succeeded while the batch layer was failing")
	}
	kv.armBatch(false)

	if height, _ := chainStore.HighestHeight(); height != 2 {
		t.Fatalf("replace failure changed the canonical height to %d", height)
	}
	if head, _ := chainStore.GetMeta("head"); head != oldHead {
		t.Fatalf("replace failure changed head metadata: %q -> %q", oldHead, head)
	}
	assertCanonicalChain(t, chainStore)
}

func TestCrashDuringDeleteBlocksAfterKeepsChain(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")
	block1 := appendCrashBlock(t, chainStore, 1, genesis.CurrentBlockHash)
	appendCrashBlock(t, chainStore, 2, block1.CurrentBlockHash)

	kv.armBatch(true)
	if err := chainStore.DeleteBlocksAfter(1); err == nil {
		t.Fatalf("DeleteBlocksAfter succeeded while the batch layer was failing")
	}
	kv.armBatch(false)

	if height, _ := chainStore.HighestHeight(); height != 2 {
		t.Fatalf("failed delete changed the canonical height to %d", height)
	}
	assertCanonicalChain(t, chainStore)
}

func TestCrashDuringPruneKeepsChain(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")
	block1 := appendCrashBlock(t, chainStore, 1, genesis.CurrentBlockHash)
	appendCrashBlock(t, chainStore, 2, block1.CurrentBlockHash)

	kv.armBatch(true)
	if _, err := chainStore.PruneBlocksBelow(2); err == nil {
		t.Fatalf("PruneBlocksBelow succeeded while the batch layer was failing")
	}
	kv.armBatch(false)

	if height, _ := chainStore.HighestHeight(); height != 2 {
		t.Fatalf("failed prune changed the canonical height to %d", height)
	}
	if _, ok := chainStore.BlockHashAt(1); !ok {
		t.Fatalf("failed prune removed canonical block 1")
	}
	if pruned, _ := chainStore.GetMeta("pruned_below"); pruned != "" {
		t.Fatalf("failed prune wrote pruned_below=%q", pruned)
	}
	assertCanonicalChain(t, chainStore)
}

func TestCrashDuringSnapshotSaveKeepsPrevious(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")
	block1 := appendCrashBlock(t, chainStore, 1, genesis.CurrentBlockHash)
	_, state, storage := crashTestBlock(t, 1, genesis.CurrentBlockHash)
	if err := chainStore.SaveSnapshot(1, block1.CurrentBlockHash, state, storage); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	kv.armBatch(true)
	if err := chainStore.SaveSnapshot(2, block1.CurrentBlockHash, state, storage); err == nil {
		t.Fatalf("SaveSnapshot succeeded while the batch layer was failing")
	}
	kv.armBatch(false)

	snapshots, err := chainStore.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 1 || toInt64(snapshots[0]["height"]) != 1 {
		t.Fatalf("failed snapshot write changed the snapshot set: %v", snapshots)
	}
}

func TestCrashDuringMetaUpdateKeepsOldValue(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	if err := chainStore.SetMeta("finalized_height", "5"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	kv.armPut(true)
	if err := chainStore.SetMeta("finalized_height", "6"); err == nil {
		t.Fatalf("SetMeta succeeded while the put layer was failing")
	}
	kv.armPut(false)

	if value, _ := chainStore.GetMeta("finalized_height"); value != "5" {
		t.Fatalf("failed meta write changed the value to %q", value)
	}
}

func TestCrashDuringReceiptsAndTxIndexKeepsChain(t *testing.T) {
	kv := interruptMemoryStore()
	chainStore := newCrashStore(t, kv)
	genesis := appendCrashBlock(t, chainStore, 0, "0")

	kv.armBatch(true)
	next, state, storage := crashTestBlock(t, 1, genesis.CurrentBlockHash)
	head := int64(1)
	err := chainStore.AppendBlock(next, AppendOptions{
		Receipts: []any{map[string]any{"tx_id": "tx-torn", "tx_index": int64(0)}},
		State:    state, Storage: storage, Head: &head,
	})
	if err == nil {
		t.Fatalf("failed append was reported as success")
	}
	kv.armBatch(false)

	if _, present, _ := chainStore.TxLookup("tx-torn"); present {
		t.Fatalf("torn append leaked a tx index entry")
	}
	if receipts, present, _ := chainStore.ReceiptsForBlock(1); present || len(receipts) != 0 {
		t.Fatalf("torn append leaked receipts")
	}
	assertCanonicalChain(t, chainStore)
}
