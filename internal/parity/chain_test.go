package parity_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

func ints64(t *testing.T, raw any) map[string]int64 {
	t.Helper()
	result := map[string]int64{}
	object, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", raw)
	}
	for key, value := range object {
		result[key] = value.(int64)
	}
	return result
}

func TestTransactionParity(t *testing.T) {
	root := fixture(t)
	section := root["transaction"].(map[string]any)

	for _, name := range []string{"transfer", "deploy"} {
		entry := section[name].(map[string]any)
		payload, ok := entry["payload"].(map[string]any)
		if !ok {
			t.Fatalf("%s payload is not an object", name)
		}
		tx, err := ledger.NewTransaction(payload, nil)
		if err != nil {
			t.Fatalf("%s: NewTransaction: %v", name, err)
		}
		if want := entry["fee"].(int64); tx.Fee != want {
			t.Errorf("%s fee: got %d, want %d", name, tx.Fee, want)
		}
		if got, want := canonicalString(t, tx.Payload), canonicalString(t, entry["payload"]); got != want {
			t.Errorf("%s payload:\n got %s\nwant %s", name, got, want)
		}
		if got, want := ledger.TxID(payload), mustString(t, entry["tx_id"]); got != want {
			t.Errorf("%s tx id: got %s, want %s", name, got, want)
		}
		if got, want := string(tx.Data), mustString(t, entry["data"]); got != want {
			t.Errorf("%s data:\n got %s\nwant %s", name, got, want)
		}
		if !ledger.VerifyTransaction(tx.Payload) {
			t.Errorf("%s: signature verification failed", name)
		}
		if want, ok := entry["serialize_hex"].(string); ok {
			message, err := ledger.SerializeMessage(payload)
			if err != nil {
				t.Fatalf("%s: serialize: %v", name, err)
			}
			if got := hex.EncodeToString(message); got != want {
				t.Errorf("%s serialized message: got %s, want %s", name, got, want)
			}
		}
	}

	for index, raw := range section["invalid"].([]any) {
		entry := raw.(map[string]any)
		payload := entry["payload"].(map[string]any)
		_, err := ledger.NewTransaction(payload, nil)
		if err == nil {
			t.Errorf("invalid[%d]: expected error", index)
			continue
		}
		if !strings.Contains(err.Error(), mustString(t, entry["error"])) {
			t.Errorf("invalid[%d]: error %q does not contain %q", index, err, entry["error"])
		}
	}
}

func TestBlockParity(t *testing.T) {
	root := fixture(t)
	section := root["blocks"].(map[string]any)

	for _, name := range []string{"b0", "b1"} {
		entry := section[name].(map[string]any)
		dict := entry["dict"].(map[string]any)
		block, err := ledger.BlockFromDict(dict)
		if err != nil {
			t.Fatalf("%s: BlockFromDict: %v", name, err)
		}
		if got, want := block.CurrentBlockHash, mustString(t, entry["hash"]); got != want {
			t.Errorf("%s hash: got %s, want %s", name, got, want)
		}
		if got, want := block.TxRoot, mustString(t, entry["tx_root"]); got != want {
			t.Errorf("%s tx root: got %s, want %s", name, got, want)
		}
		if got, want := canonicalString(t, block.ToHeaderDict()), canonicalString(t, entry["header"]); got != want {
			t.Errorf("%s header:\n got %s\nwant %s", name, got, want)
		}
		if got, want := canonicalString(t, block.ToDict()), canonicalString(t, entry["dict"]); got != want {
			t.Errorf("%s dict:\n got %s\nwant %s", name, got, want)
		}
	}

	tampered := section["tampered"].(map[string]any)
	_, err := ledger.BlockFromDict(tampered["dict"].(map[string]any))
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("tampered block: expected hash mismatch, got %v", err)
	}
}

func TestBlockchainParity(t *testing.T) {
	root := fixture(t)
	section := root["chain"].(map[string]any)

	config := ledger.DefaultBlockchainConfig()
	config.ChainID = "san-devnet-1"
	config.GenesisBalances = ints64(t, section["genesis_allocations"])
	chain := ledger.NewBlockchain(config)

	if got, want := chain.GenesisStateRoot, mustString(t, section["genesis_state_root"]); got != want {
		t.Errorf("genesis state root: got %s, want %s", got, want)
	}
	if got, want := chain.Tip().CurrentBlockHash, mustString(t, section["genesis_hash"]); got != want {
		t.Errorf("genesis hash: got %s, want %s", got, want)
	}
	if got, want := canonicalString(t, intMapToAny(chain.Parameters)), canonicalString(t, section["parameters"]); got != want {
		t.Errorf("parameters:\n got %s\nwant %s", got, want)
	}
	if got, want := canonicalString(t, intMapToAny(chain.GenesisAllocations)), canonicalString(t, section["genesis_allocations"]); got != want {
		t.Errorf("genesis allocations:\n got %s\nwant %s", got, want)
	}
	if got, want := chain.NextFeeRate(), section["next_fee_rate"].(int64); got != want {
		t.Errorf("next fee rate: got %d, want %d", got, want)
	}

	medianConfig := ledger.DefaultBlockchainConfig()
	medianConfig.ChainID = "san-devnet-1"
	medianConfig.GenesisBalances = map[string]int64{}
	median := ledger.NewBlockchain(medianConfig)
	timestamps := []float64{0.0, 1.5, 2.5, 4.0}
	median.Chain = make([]*ledger.Block, len(timestamps))
	for i, timestamp := range timestamps {
		median.Chain[i] = ledger.NewBlock(int64(i), "0", "v", "s", []any{},
			timestamp, "san-devnet-1", nil, 0, nil)
	}
	for _, raw := range section["median"].([]any) {
		entry := raw.(map[string]any)
		window := int(entry["window"].(int64))
		got := median.MedianTimePast(window)
		want := entry["value"].(float64)
		if got != want {
			t.Errorf("median(window=%d): got %v, want %v", window, got, want)
		}
	}
}

func intMapToAny(input map[string]int64) map[string]any {
	result := map[string]any{}
	for key, value := range input {
		result[key] = value
	}
	return result
}

func TestChainStoreParity(t *testing.T) {
	root := fixture(t)
	section := root["chainstore"].(map[string]any)

	for _, name := range []string{"a", "b"} {
		entry := section[name].(map[string]any)
		memory := store.NewMemoryStore(":memory:")
		chainStore, err := ledger.NewChainStore(":memory:", "memory", memory)
		if err != nil {
			t.Fatalf("%s: NewChainStore: %v", name, err)
		}

		b0, b1, b2 := storeBlocks()
		switch name {
		case "a":
			state := storeState()
			storage := storeStorage()
			if err := chainStore.AppendBlock(b0, ledger.AppendOptions{
				Receipts: []any{}, State: state, Storage: storage, Head: int64Pointer(0)}); err != nil {
				t.Fatalf("append b0: %v", err)
			}
			if err := chainStore.AppendBlock(b1, ledger.AppendOptions{
				Receipts: []any{map[string]any{"tx_id": "tx1", "tx_index": int64(0)}},
				State:    state, Storage: storage, Head: int64Pointer(1)}); err != nil {
				t.Fatalf("append b1: %v", err)
			}
			if err := chainStore.AppendBlock(b2, ledger.AppendOptions{
				Receipts: []any{}, State: state, Storage: storage, Head: int64Pointer(2)}); err != nil {
				t.Fatalf("append b2: %v", err)
			}
		case "b":
			if err := chainStore.AppendBlock(b0, ledger.AppendOptions{Receipts: []any{}, Head: int64Pointer(0)}); err != nil {
				t.Fatalf("append b0: %v", err)
			}
			if err := chainStore.AppendBlock(b1, ledger.AppendOptions{
				Receipts: []any{map[string]any{"tx_id": "tx1", "tx_index": int64(0)}}, Head: int64Pointer(1)}); err != nil {
				t.Fatalf("append b1: %v", err)
			}
			if err := chainStore.AppendBlock(b2, ledger.AppendOptions{Receipts: []any{}, Head: int64Pointer(2)}); err != nil {
				t.Fatalf("append b2: %v", err)
			}
			if err := chainStore.ReplaceChain([]*ledger.Block{b0, b1, b2},
				storeState(), storeStorage(), map[int64][]any{
					1: {map[string]any{"tx_id": "tx1", "tx_index": int64(0)}},
				}); err != nil {
				t.Fatalf("replace chain: %v", err)
			}
			if err := chainStore.SaveSnapshot(1, b1.CurrentBlockHash, storeState(), storeStorage()); err != nil {
				t.Fatalf("save snapshot 1: %v", err)
			}
			if err := chainStore.SaveSnapshot(2, strings.Repeat("0", 64), storeState(), storeStorage()); err != nil {
				t.Fatalf("save snapshot 2: %v", err)
			}
			removed, err := chainStore.DeleteMismatchedSnapshots(func(height int64) (string, bool) {
				if height == 1 {
					return b1.CurrentBlockHash, true
				}
				return "", false
			})
			if err != nil {
				t.Fatalf("delete mismatched snapshots: %v", err)
			}
			if want := entry["snapshots_removed"].(int64); int64(removed) != want {
				t.Errorf("%s snapshots removed: got %d, want %d", name, removed, want)
			}
			if err := chainStore.DeleteBlocksAfter(1); err != nil {
				t.Fatalf("delete blocks after: %v", err)
			}
			if _, err := chainStore.PruneBlocksBelow(2); err != nil {
				t.Fatalf("prune blocks below: %v", err)
			}
		}

		expected := map[string]string{}
		for _, pair := range entry["pairs"].([]any) {
			values := pair.([]any)
			expected[values[0].(string)] = values[1].(string)
		}
		actual := map[string]string{}
		for key, value := range memory.Snapshot() {
			actual[hex.EncodeToString([]byte(key))] = hex.EncodeToString(value)
		}
		if len(actual) != len(expected) {
			t.Errorf("%s: store has %d keys, want %d", name, len(actual), len(expected))
		}
		for key, want := range expected {
			got, ok := actual[key]
			if !ok {
				t.Errorf("%s: missing key %s", name, key)
				continue
			}
			if got != want {
				t.Errorf("%s: value mismatch for key %s:\n got %s\nwant %s", name, key, got, want)
			}
		}

		meta := entry["meta"].(map[string]any)
		for metaKey, want := range meta {
			got, ok := chainStore.GetMeta(metaKey)
			if want == nil {
				if ok {
					t.Errorf("%s: meta %s should be absent, got %q", name, metaKey, got)
				}
				continue
			}
			if !ok || got != want.(string) {
				t.Errorf("%s: meta %s: got %q (present %v), want %q", name, metaKey, got, ok, want)
			}
		}
		if height, ok := chainStore.HighestHeight(); !ok || height != entry["highest_height"].(int64) {
			t.Errorf("%s: highest height: got %d (present %v)", name, height, ok)
		}
		if got, want := int64(chainStore.CountBlocks()), entry["count_blocks"].(int64); got != want {
			t.Errorf("%s: count blocks: got %d, want %d", name, got, want)
		}

		if name == "a" {
			state, err := chainStore.LoadState()
			if err != nil {
				t.Fatalf("load state: %v", err)
			}
			if got, want := canonicalString(t, state), mustString(t, entry["state"]); got != want {
				t.Errorf("state:\n got %s\nwant %s", got, want)
			}
			storage, err := chainStore.LoadStorage()
			if err != nil {
				t.Fatalf("load storage: %v", err)
			}
			if got, want := canonicalString(t, storage), mustString(t, entry["storage"]); got != want {
				t.Errorf("storage:\n got %s\nwant %s", got, want)
			}
			receipts, _, err := chainStore.ReceiptsForBlock(1)
			if err != nil {
				t.Fatalf("receipts: %v", err)
			}
			if got, want := canonicalString(t, receipts), mustString(t, entry["receipts_at_1"]); got != want {
				t.Errorf("receipts:\n got %s\nwant %s", got, want)
			}
			lookup, ok, err := chainStore.TxLookup("tx1")
			if err != nil || !ok {
				t.Fatalf("tx lookup: ok=%v err=%v", ok, err)
			}
			if got, want := canonicalString(t, lookup), mustString(t, entry["tx_lookup"]); got != want {
				t.Errorf("tx lookup:\n got %s\nwant %s", got, want)
			}
			chain, err := chainStore.LoadChain(0, nil)
			if err != nil {
				t.Fatalf("load chain: %v", err)
			}
			if got, want := int64(len(chain)), entry["chain_len"].(int64); got != want {
				t.Errorf("chain length: got %d, want %d", got, want)
			}
			block1, err := chainStore.LoadBlock(1)
			if err != nil || block1 == nil {
				t.Fatalf("load block 1: %v", err)
			}
			if got, want := block1.CurrentBlockHash, mustString(t, entry["block1_hash"]); got != want {
				t.Errorf("block1 hash: got %s, want %s", got, want)
			}
		} else {
			snapshots, err := chainStore.ListSnapshots()
			if err != nil {
				t.Fatalf("list snapshots: %v", err)
			}
			if got, want := canonicalString(t, snapshots), canonicalString(t, entry["snapshots"]); got != want {
				t.Errorf("snapshots:\n got %s\nwant %s", got, want)
			}
			latest, err := chainStore.LoadLatestSnapshot()
			if err != nil || latest == nil {
				t.Fatalf("latest snapshot: %v", err)
			}
			if got, want := toI64Any(latest["height"]), entry["latest_snapshot_height"].(int64); got != want {
				t.Errorf("latest snapshot height: got %d, want %d", got, want)
			}
		}
	}
}

func toI64Any(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func int64Pointer(value int64) *int64 { return &value }

func storeBlocks() (*ledger.Block, *ledger.Block, *ledger.Block) {
	chainID := "san-devnet-1"
	b0 := ledger.NewBlock(0, "0", "GENESIS_VALIDATOR", "GENESIS_SIGNATURE",
		[]any{ledger.GenesisMessage}, 0.0, chainID, "root0", 0, nil)
	b1 := ledger.NewBlock(1, b0.CurrentBlockHash, "v1", "sig1",
		[]any{map[string]any{"chain_id": chainID, "sender": "aa", "nonce": int64(0), "value": int64(5)}},
		100.5, chainID, "root1", 0, nil)
	b2 := ledger.NewBlock(2, b1.CurrentBlockHash, "v1", "sig2", []any{},
		200.25, chainID, "root2", 0, nil)
	return b0, b1, b2
}

func storeState() map[string]any {
	return map[string]any{
		"balances":      map[string]any{"0xaa": int64(5)},
		"nonces":        map[string]any{"0xaa": int64(1)},
		"validators":    map[string]any{},
		"total_slashed": int64(0),
		"total_burned":  int64(2),
		"base_fee":      int64(3),
		"parameters":    map[string]any{"slash_bps": int64(5000)},
	}
}

func storeStorage() map[string]any {
	return map[string]any{
		"data":      map[string]any{"x": int64(1)},
		"functions": map[string]any{},
		"contracts": map[string]any{},
	}
}
