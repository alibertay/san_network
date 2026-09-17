package ledger

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// Chain persistence over a namespaced key-value store.
//
// Layout (single LMDB file, byte prefixes act as column families):
//
//	m:<name>            metadata (schema version, finalized checkpoint, genesis)
//	h:<block_hash>      block header (hashed fields + signature)
//	b:<block_hash>      block body (transactions)
//	n:<height>          canonical index: height -> block hash
//	r:<block_hash>      execution receipts
//	t:<tx_id>           transaction index: tx id -> {block, position}
//	S:state             ledger state snapshot (stored as m:__state)
//	S:storage           contract storage snapshot (stored as m:__storage)
//	k:<height>          finalized state snapshots
const (
	StoreSchemaVersion = 1
	metaPrefix         = "m:"
	headerPrefix       = "h:"
	bodyPrefix         = "b:"
	canonPrefix        = "n:"
	receiptsPrefix     = "r:"
	txPrefix           = "t:"
	stateKey           = "m:__state"
	storageKey         = "m:__storage"
	snapshotPrefix     = "k:"
)

// ChainStore stores blocks, state and receipts with atomic per-block commits.
type ChainStore struct {
	Path string
	kv   store.KeyValueStore
	mu   sync.RWMutex
}

// NewChainStore opens the store (kv may be nil to select a backend).
func NewChainStore(path, backend string, kv store.KeyValueStore) (*ChainStore, error) {
	if kv == nil {
		opened, err := store.Open(path, backend)
		if err != nil {
			return nil, err
		}
		kv = opened
	}
	chainStore := &ChainStore{Path: path, kv: kv}
	if err := chainStore.ensureSchema(); err != nil {
		return nil, err
	}
	return chainStore, nil
}

// KeyValue returns the underlying key-value store (tests, migrations).
func (chainStore *ChainStore) KeyValue() store.KeyValueStore {
	return chainStore.kv
}

// GetMeta reads a metadata value.
func (chainStore *ChainStore) GetMeta(key string) (string, bool) {
	raw, ok := chainStore.kv.Get([]byte(metaPrefix + key))
	if !ok {
		return "", false
	}
	return string(raw), true
}

// SetMeta writes a metadata value.
func (chainStore *ChainStore) SetMeta(key, value string) error {
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.Put([]byte(metaPrefix+key), []byte(value))
}

func (chainStore *ChainStore) ensureSchema() error {
	raw, ok := chainStore.kv.Get([]byte(metaPrefix + "schema_version"))
	if !ok {
		return chainStore.kv.Put([]byte(metaPrefix+"schema_version"),
			[]byte(strconv.Itoa(StoreSchemaVersion)))
	}
	version, err := strconv.Atoi(string(raw))
	if err != nil {
		return &store.SchemaMismatch{StorageError: store.StorageError{
			Message: fmt.Sprintf("Invalid schema version marker: %q", raw)}}
	}
	if version > StoreSchemaVersion {
		return &store.SchemaMismatch{StorageError: store.StorageError{Message: fmt.Sprintf(
			"On-disk schema %d is newer than this build (%d); upgrade the node or point SAN_DB_PATH at a fresh database",
			version, StoreSchemaVersion)}}
	}
	return nil
}

func dump(payload any) ([]byte, error) {
	return canonical.Marshal(payload)
}

func load(raw []byte) (any, error) {
	return canonical.Decode(raw)
}

func heightKey(height int64) []byte {
	return []byte(fmt.Sprintf("n:%012d", height))
}

func snapshotKey(height int64) []byte {
	return []byte(fmt.Sprintf("k:%012d", height))
}

// TransactionID is the replay-protection id used by the tx index.
func TransactionID(tx map[string]any) string {
	return TxID(tx)
}

// ---------------------------------------------------------------------- #
// Blocks
// ---------------------------------------------------------------------- #

// IsEmpty reports whether the store has no canonical blocks.
func (chainStore *ChainStore) IsEmpty() bool {
	_, ok := chainStore.HighestHeight()
	return !ok
}

// HighestHeight returns the highest stored block height.
func (chainStore *ChainStore) HighestHeight() (int64, bool) {
	pairs := chainStore.kv.PrefixIterator([]byte(canonPrefix), nil)
	if len(pairs) == 0 {
		return 0, false
	}
	last := pairs[len(pairs)-1].Key
	height, err := strconv.ParseInt(string(last[len(last)-12:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return height, true
}

// BlockHashAt returns the canonical hash at a height.
func (chainStore *ChainStore) BlockHashAt(height int64) (string, bool) {
	raw, ok := chainStore.kv.Get(heightKey(height))
	if !ok {
		return "", false
	}
	return string(raw), true
}

// BlockByHash loads a block by hash.
func (chainStore *ChainStore) BlockByHash(blockHash string) (*Block, error) {
	headerRaw, ok := chainStore.kv.Get([]byte(headerPrefix + blockHash))
	if !ok {
		return nil, nil
	}
	bodyRaw, hasBody := chainStore.kv.Get([]byte(bodyPrefix + blockHash))
	payloadValue, err := load(headerRaw)
	if err != nil {
		return nil, err
	}
	payload, ok := payloadValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stored block header is not an object")
	}
	transactions := []any{}
	if hasBody {
		bodyValue, err := load(bodyRaw)
		if err != nil {
			return nil, err
		}
		if list, ok := bodyValue.([]any); ok {
			transactions = list
		}
	}
	payload["transactions"] = transactions
	return BlockFromDict(payload)
}

// LoadBlock loads the canonical block at a height.
func (chainStore *ChainStore) LoadBlock(height int64) (*Block, error) {
	blockHash, ok := chainStore.BlockHashAt(height)
	if !ok {
		return nil, nil
	}
	return chainStore.BlockByHash(blockHash)
}

// LoadChain loads canonical blocks starting at fromHeight.
func (chainStore *ChainStore) LoadChain(fromHeight int64, limit *int64) ([]*Block, error) {
	chain := []*Block{}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(canonPrefix), heightKey(fromHeight)) {
		block, err := chainStore.BlockByHash(string(pair.Value))
		if err != nil {
			return nil, err
		}
		if block == nil {
			continue
		}
		chain = append(chain, block)
		if limit != nil && int64(len(chain)) >= *limit {
			break
		}
	}
	return chain, nil
}

// CountBlocks counts canonical entries.
func (chainStore *ChainStore) CountBlocks() int {
	return len(chainStore.kv.PrefixIterator([]byte(canonPrefix), nil))
}

// AppendOptions carries optional per-block data.
type AppendOptions struct {
	Receipts []any
	State    map[string]any
	Storage  map[string]any
	Head     *int64
}

// AppendBlock atomically stores a block plus its indexes, receipts and state.
func (chainStore *ChainStore) AppendBlock(block *Block, options AppendOptions) error {
	blockHash := block.CurrentBlockHash
	batch := &store.WriteBatch{}

	header, err := dump(block.ToHeaderDict())
	if err != nil {
		return err
	}
	body, err := dump(block.transactionDicts())
	if err != nil {
		return err
	}
	batch.Put([]byte(headerPrefix+blockHash), header)
	batch.Put([]byte(bodyPrefix+blockHash), body)
	batch.Put(heightKey(block.Index), []byte(blockHash))

	if options.Receipts != nil {
		if err := addReceipts(batch, blockHash, block.Index, options.Receipts); err != nil {
			return err
		}
	}
	if options.State != nil {
		state, err := dump(options.State)
		if err != nil {
			return err
		}
		batch.Put([]byte(stateKey), state)
	}
	if options.Storage != nil {
		storage, err := dump(options.Storage)
		if err != nil {
			return err
		}
		batch.Put([]byte(storageKey), storage)
	}

	head := block.Index
	if options.Head != nil {
		head = *options.Head
	}
	batch.Put([]byte(metaPrefix+"head"), []byte(strconv.FormatInt(head, 10)))

	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.WriteBatch(batch)
}

func addReceipts(batch *store.WriteBatch, blockHash string, index int64, receipts []any) error {
	encoded, err := dump(receipts)
	if err != nil {
		return err
	}
	batch.Put([]byte(receiptsPrefix+blockHash), encoded)
	for _, raw := range receipts {
		receipt, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		txID, _ := receipt["tx_id"].(string)
		if txID == "" {
			continue
		}
		entry, err := dump(map[string]any{
			"block_hash":  blockHash,
			"block_index": index,
			"tx_index":    receipt["tx_index"],
		})
		if err != nil {
			return err
		}
		batch.Put([]byte(txPrefix+txID), entry)
	}
	return nil
}

// SaveBlock appends a block without state/receipts.
func (chainStore *ChainStore) SaveBlock(block *Block) error {
	return chainStore.AppendBlock(block, AppendOptions{})
}

// ReplaceChain atomically replaces the stored chain (reorgs and replay).
func (chainStore *ChainStore) ReplaceChain(blocks []*Block, state, storage map[string]any,
	receiptsByIndex map[int64][]any) error {
	batch := &store.WriteBatch{}

	oldReceipts := map[string][]byte{}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(receiptsPrefix), nil) {
		oldReceipts[string(pair.Key)] = pair.Value
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(canonPrefix), nil) {
		batch.Delete(pair.Key)
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(headerPrefix), nil) {
		batch.Delete(pair.Key)
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(bodyPrefix), nil) {
		batch.Delete(pair.Key)
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(receiptsPrefix), nil) {
		batch.Delete(pair.Key)
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(txPrefix), nil) {
		batch.Delete(pair.Key)
	}

	for _, block := range blocks {
		blockHash := block.CurrentBlockHash
		header, err := dump(block.ToHeaderDict())
		if err != nil {
			return err
		}
		body, err := dump(block.transactionDicts())
		if err != nil {
			return err
		}
		batch.Put([]byte(headerPrefix+blockHash), header)
		batch.Put([]byte(bodyPrefix+blockHash), body)
		batch.Put(heightKey(block.Index), []byte(blockHash))

		if receipts, ok := receiptsByIndex[block.Index]; ok {
			if err := addReceipts(batch, blockHash, block.Index, receipts); err != nil {
				return err
			}
			continue
		}
		if raw, ok := oldReceipts[receiptsPrefix+blockHash]; ok {
			value, err := load(raw)
			if err != nil {
				continue
			}
			if stored, ok := value.([]any); ok {
				if err := addReceipts(batch, blockHash, block.Index, stored); err != nil {
					return err
				}
			}
		}
	}

	if len(blocks) > 0 {
		batch.Put([]byte(metaPrefix+"head"), []byte(strconv.FormatInt(blocks[len(blocks)-1].Index, 10)))
	}
	if state != nil {
		encoded, err := dump(state)
		if err != nil {
			return err
		}
		batch.Put([]byte(stateKey), encoded)
	}
	if storage != nil {
		encoded, err := dump(storage)
		if err != nil {
			return err
		}
		batch.Put([]byte(storageKey), encoded)
	}

	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.WriteBatch(batch)
}

// DeleteBlocksAfter removes blocks above a height.
func (chainStore *ChainStore) DeleteBlocksAfter(height int64) error {
	batch := &store.WriteBatch{}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(canonPrefix), nil) {
		keyHeight, err := strconv.ParseInt(string(pair.Key[len(pair.Key)-12:]), 10, 64)
		if err != nil || keyHeight <= height {
			continue
		}
		blockHash := string(pair.Value)
		batch.Delete(pair.Key)
		batch.Delete([]byte(headerPrefix + blockHash))
		batch.Delete([]byte(bodyPrefix + blockHash))
		batch.Delete([]byte(receiptsPrefix + blockHash))
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(txPrefix), nil) {
		value, err := load(pair.Value)
		if err != nil {
			continue
		}
		record, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if toInt64(record["block_index"]) > height {
			batch.Delete(pair.Key)
		}
	}
	batch.Put([]byte(metaPrefix+"head"), []byte(strconv.FormatInt(height, 10)))
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.WriteBatch(batch)
}

// PruneBlocksBelow drops block bodies/indexes below height (genesis is kept).
func (chainStore *ChainStore) PruneBlocksBelow(height int64) (int, error) {
	removed := 0
	batch := &store.WriteBatch{}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(canonPrefix), nil) {
		blockHeight, err := strconv.ParseInt(string(pair.Key[len(pair.Key)-12:]), 10, 64)
		if err != nil || blockHeight == 0 || blockHeight >= height {
			continue
		}
		blockHash := string(pair.Value)
		batch.Delete(pair.Key)
		batch.Delete([]byte(headerPrefix + blockHash))
		batch.Delete([]byte(bodyPrefix + blockHash))
		batch.Delete([]byte(receiptsPrefix + blockHash))
		removed++
	}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(txPrefix), nil) {
		value, err := load(pair.Value)
		if err != nil {
			continue
		}
		record, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if toInt64(record["block_index"]) < height {
			batch.Delete(pair.Key)
		}
	}
	batch.Put([]byte(metaPrefix+"pruned_below"), []byte(strconv.FormatInt(height, 10)))
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	if err := chainStore.kv.WriteBatch(batch); err != nil {
		return 0, err
	}
	return removed, nil
}

// ---------------------------------------------------------------------- #
// Receipts and transaction index
// ---------------------------------------------------------------------- #

// ReceiptsForBlock returns the receipts stored for a height.
func (chainStore *ChainStore) ReceiptsForBlock(height int64) ([]any, bool, error) {
	blockHash, ok := chainStore.BlockHashAt(height)
	if !ok {
		return nil, false, nil
	}
	raw, ok := chainStore.kv.Get([]byte(receiptsPrefix + blockHash))
	if !ok {
		return nil, false, nil
	}
	value, err := load(raw)
	if err != nil {
		return nil, false, err
	}
	receipts, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("stored receipts are not a list")
	}
	return receipts, true, nil
}

// TxLookup resolves a transaction id to its block and position.
func (chainStore *ChainStore) TxLookup(txID string) (map[string]any, bool, error) {
	raw, ok := chainStore.kv.Get([]byte(txPrefix + txID))
	if !ok {
		return nil, false, nil
	}
	value, err := load(raw)
	if err != nil {
		return nil, false, err
	}
	record, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("stored tx index entry is not an object")
	}
	return record, true, nil
}

// ---------------------------------------------------------------------- #
// Ledger state
// ---------------------------------------------------------------------- #

// SaveState stores the ledger state snapshot.
func (chainStore *ChainStore) SaveState(state map[string]any) error {
	encoded, err := dump(state)
	if err != nil {
		return err
	}
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.Put([]byte(stateKey), encoded)
}

// LoadState returns the ledger state snapshot.
func (chainStore *ChainStore) LoadState() (map[string]any, error) {
	raw, ok := chainStore.kv.Get([]byte(stateKey))
	if !ok {
		return nil, nil
	}
	value, err := load(raw)
	if err != nil {
		return nil, err
	}
	state, _ := value.(map[string]any)
	return state, nil
}

// SaveStorage stores the contract storage snapshot.
func (chainStore *ChainStore) SaveStorage(storage map[string]any) error {
	encoded, err := dump(storage)
	if err != nil {
		return err
	}
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.Put([]byte(storageKey), encoded)
}

// LoadStorage returns the contract storage snapshot.
func (chainStore *ChainStore) LoadStorage() (map[string]any, error) {
	raw, ok := chainStore.kv.Get([]byte(storageKey))
	if !ok {
		return nil, nil
	}
	value, err := load(raw)
	if err != nil {
		return nil, err
	}
	storage, _ := value.(map[string]any)
	return storage, nil
}

// ---------------------------------------------------------------------- #
// Snapshots
// ---------------------------------------------------------------------- #

// SaveSnapshot stores a finalized state snapshot.
func (chainStore *ChainStore) SaveSnapshot(height int64, blockHash string, state, storage map[string]any) error {
	payload, err := dump(map[string]any{"hash": blockHash, "state": state, "storage": storage})
	if err != nil {
		return err
	}
	batch := &store.WriteBatch{}
	batch.Put(snapshotKey(height), payload)
	batch.Put([]byte(metaPrefix+"snapshot_height"), []byte(strconv.FormatInt(height, 10)))
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	return chainStore.kv.WriteBatch(batch)
}

// ListSnapshots returns all snapshots, newest first.
func (chainStore *ChainStore) ListSnapshots() ([]map[string]any, error) {
	snapshots := []map[string]any{}
	for _, pair := range chainStore.kv.PrefixIterator([]byte(snapshotPrefix), nil) {
		value, err := load(pair.Value)
		if err != nil {
			return nil, err
		}
		payload, ok := value.(map[string]any)
		if !ok {
			continue
		}
		height, err := strconv.ParseInt(string(pair.Key[len(snapshotPrefix):]), 10, 64)
		if err != nil {
			continue
		}
		snapshots = append(snapshots, map[string]any{
			"height": height, "hash": payload["hash"],
			"state": payload["state"], "storage": payload["storage"],
		})
	}
	for i := 1; i < len(snapshots); i++ {
		for j := i; j > 0 && toInt64(snapshots[j]["height"]) > toInt64(snapshots[j-1]["height"]); j-- {
			snapshots[j], snapshots[j-1] = snapshots[j-1], snapshots[j]
		}
	}
	return snapshots, nil
}

// DeleteMismatchedSnapshots drops snapshots that are not on the canonical chain.
func (chainStore *ChainStore) DeleteMismatchedSnapshots(canonicalHashAt func(int64) (string, bool)) (int, error) {
	removed := 0
	batch := &store.WriteBatch{}
	snapshots, err := chainStore.ListSnapshots()
	if err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		height := toInt64(snapshot["height"])
		hash, ok := canonicalHashAt(height)
		if ok && hash == snapshot["hash"] {
			continue
		}
		batch.Delete(snapshotKey(height))
		removed++
	}
	if removed == 0 {
		return 0, nil
	}
	chainStore.mu.Lock()
	defer chainStore.mu.Unlock()
	if err := chainStore.kv.WriteBatch(batch); err != nil {
		return 0, err
	}
	return removed, nil
}

// LoadLatestSnapshot returns the highest snapshot.
func (chainStore *ChainStore) LoadLatestSnapshot() (map[string]any, error) {
	pairs := chainStore.kv.PrefixIterator([]byte(snapshotPrefix), nil)
	if len(pairs) == 0 {
		return nil, nil
	}
	last := pairs[len(pairs)-1]
	raw, ok := chainStore.kv.Get(last.Key)
	if !ok {
		return nil, nil
	}
	value, err := load(raw)
	if err != nil {
		return nil, err
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	height, err := strconv.ParseInt(string(last.Key[len(snapshotPrefix):]), 10, 64)
	if err != nil {
		return nil, nil
	}
	return map[string]any{
		"height": height, "hash": payload["hash"],
		"state": payload["state"], "storage": payload["storage"],
	}, nil
}

// ---------------------------------------------------------------------- #
// Lifecycle
// ---------------------------------------------------------------------- #

// Flush forces durability.
func (chainStore *ChainStore) Flush() error { return chainStore.kv.Flush() }

// Close closes the backing store.
func (chainStore *ChainStore) Close() error { return chainStore.kv.Close() }
