package ledger

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/alibertay/san_network/internal/canonical"

	"golang.org/x/crypto/sha3"
)

// Bump when the serialized block/transaction shape changes.
// 2: chain_id became part of the hashed block header.
// 3: tx_root and state_root became part of the hashed block header.
// 4: round (proposer fallback) became part of the hashed block header.
// 5: reward_address became part of the hashed block header.
const SchemaVersion = 5

// Block mirrors blockchain/Block.py.
type Block struct {
	ChainID            any
	Index              int64
	PreviousBlockHash  string
	Timestamp          any // int64 or float64; part of the hashed header
	Validator          any
	ValidatorSignature any
	Transactions       []any
	TxRoot             string
	StateRoot          any
	Round              int64
	RewardAddress      any
	CurrentBlockHash   string
}

// NewBlock builds a block and computes its tx root and hash.
func NewBlock(index int64, previousBlockHash string, validator, validatorSignature any,
	transactions []any, timestamp any, chainID, stateRoot any, round int64,
	rewardAddress any) *Block {
	if timestamp == nil {
		timestamp = float64(time.Now().UnixNano()) / 1e9
	}
	if transactions == nil {
		transactions = []any{}
	}

	txRoot := EmptyRoot
	if len(transactions) > 0 {
		hashes := make([]any, len(transactions))
		for i, tx := range transactions {
			hashes[i] = transactionToDict(tx)
		}
		txRoot = MerkleRoot(hashes)
	}

	block := &Block{
		ChainID:            chainID,
		Index:              index,
		PreviousBlockHash:  previousBlockHash,
		Timestamp:          timestamp,
		Validator:          validator,
		ValidatorSignature: validatorSignature,
		Transactions:       transactions,
		TxRoot:             txRoot,
		StateRoot:          stateRoot,
		Round:              round,
		RewardAddress:      rewardAddress,
	}
	block.CurrentBlockHash = block.CalculateHash()
	return block
}

func transactionToDict(transaction any) any {
	switch value := transaction.(type) {
	case map[string]any:
		return value
	case *Transaction:
		return value.Payload
	case Transaction:
		return value.Payload
	default:
		return map[string]any{"data": pythonStr(transaction)}
	}
}

func pythonStr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	default:
		text, err := canonical.MarshalString(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return text
	}
}

func (block *Block) transactionDicts() []any {
	transactionDicts := make([]any, len(block.Transactions))
	for i, tx := range block.Transactions {
		transactionDicts[i] = transactionToDict(tx)
	}
	return transactionDicts
}

// HeaderDict is everything the block hash commits to.
func (block *Block) HeaderDict() map[string]any {
	return map[string]any{
		"chain_id":            block.ChainID,
		"index":               block.Index,
		"previous_block_hash": block.PreviousBlockHash,
		"timestamp":           block.Timestamp,
		"validator":           block.Validator,
		"transactions":        block.transactionDicts(),
		"tx_root":             block.TxRoot,
		"state_root":          block.StateRoot,
		"round":               block.Round,
		"reward_address":      block.RewardAddress,
	}
}

// CalculateHash is the deterministic SHA3-256 hash over the canonical header.
func (block *Block) CalculateHash() string {
	data, err := canonical.Marshal(block.HeaderDict())
	if err != nil {
		return ""
	}
	digest := sha3.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// ToDict is the full block representation (header + signature + hash + version).
func (block *Block) ToDict() map[string]any {
	result := block.HeaderDict()
	result["validator_signature"] = block.ValidatorSignature
	result["current_block_hash"] = block.CurrentBlockHash
	result["version"] = int64(SchemaVersion)
	return result
}

// ToHeaderDict is the light-client header.
func (block *Block) ToHeaderDict() map[string]any {
	return map[string]any{
		"version":             int64(SchemaVersion),
		"chain_id":            block.ChainID,
		"index":               block.Index,
		"previous_block_hash": block.PreviousBlockHash,
		"timestamp":           block.Timestamp,
		"validator":           block.Validator,
		"validator_signature": block.ValidatorSignature,
		"tx_root":             block.TxRoot,
		"state_root":          block.StateRoot,
		"round":               block.Round,
		"reward_address":      block.RewardAddress,
		"current_block_hash":  block.CurrentBlockHash,
	}
}

// BlockFromDict validates and rebuilds a block from its JSON representation.
func BlockFromDict(data map[string]any) (*Block, error) {
	if version, ok := data["version"]; ok && version != nil {
		parsed, err := intField(data, "version")
		if err != nil {
			return nil, fmt.Errorf("Invalid block schema version: %v", version)
		}
		if parsed > SchemaVersion {
			return nil, fmt.Errorf(
				"Unsupported block schema version %d (this node supports %d)", parsed, SchemaVersion)
		}
	}

	index, err := intField(data, "index")
	if err != nil {
		return nil, fmt.Errorf("block index is required")
	}
	previous, _ := data["previous_block_hash"].(string)
	timestamp := data["timestamp"]

	transactions := []any{}
	if raw, ok := data["transactions"].([]any); ok {
		transactions = raw
	}

	round, _ := intField(data, "round")

	block := NewBlock(
		index,
		previous,
		data["validator"],
		data["validator_signature"],
		transactions,
		timestamp,
		data["chain_id"],
		data["state_root"],
		round,
		data["reward_address"],
	)

	if announced, ok := data["tx_root"]; ok && announced != nil {
		if announced != block.TxRoot {
			return nil, fmt.Errorf("Block %d: tx_root mismatch (announced %v, computed %s)",
				block.Index, announced, block.TxRoot)
		}
	}
	if expected, ok := data["current_block_hash"]; ok && expected != nil {
		if expected != block.CurrentBlockHash {
			return nil, fmt.Errorf("Block %d: hash mismatch (announced %v, computed %s)",
				block.Index, expected, block.CurrentBlockHash)
		}
	}
	return block, nil
}

// TimestampFloat returns the timestamp as a float64 (median-time helpers).
func (block *Block) TimestampFloat() float64 {
	switch value := block.Timestamp.(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	case int:
		return float64(value)
	case *big.Int:
		if value != nil && value.IsInt64() {
			return float64(value.Int64())
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

// HasNumericTimestamp reports whether the timestamp is a real number, so the
// consensus checks (median time, drift, minimum interval) cannot be bypassed
// with a non-numeric or NaN timestamp.
func (block *Block) HasNumericTimestamp() bool {
	value := block.TimestampFloat()
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
