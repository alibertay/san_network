package ledger

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/alibertay/san_network/internal/canonical"

	"golang.org/x/crypto/sha3"
)

// Merkle trees for transaction and state commitments. Both trees use
// SHA3-256, canonical JSON leaves and node promotion on odd levels (no
// duplicated hashes), so roots and proofs are deterministic on every node.

// EmptyRoot is the fixed root of an empty tree.
var EmptyRoot = func() string {
	digest := sha3.Sum256([]byte("san-empty-tree"))
	return hex.EncodeToString(digest[:])
}()

// LeafHash hashes a leaf value (canonical JSON for anything non-string).
func LeafHash(leaf any) string {
	var payload []byte
	switch value := leaf.(type) {
	case []byte:
		payload = value
	case string:
		payload = []byte(value)
	default:
		data, err := canonical.Marshal(leaf)
		if err != nil {
			payload = []byte(fmt.Sprintf("%v", leaf))
		} else {
			payload = data
		}
	}
	digest := sha3.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func parentHash(left, right string) string {
	digest := sha3.Sum256([]byte(left + right))
	return hex.EncodeToString(digest[:])
}

// MerkleRoot returns the root of the Merkle tree over leaves.
func MerkleRoot(leaves []any) string {
	level := make([]string, len(leaves))
	for i, leaf := range leaves {
		level[i] = LeafHash(leaf)
	}
	if len(level) == 0 {
		return EmptyRoot
	}
	for len(level) > 1 {
		next := make([]string, 0, (len(level)+1)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, parentHash(level[i], level[i+1]))
		}
		if len(level)%2 == 1 {
			next = append(next, level[len(level)-1]) // promote the odd node
		}
		level = next
	}
	return level[0]
}

// MerkleProof returns the inclusion proof for leaves[index]. Each step is
// {"position": "left"|"right", "hash": ...} where position is the side of
// the provided hash.
func MerkleProof(leaves []any, index int) ([]map[string]any, error) {
	if index < 0 || index >= len(leaves) {
		return nil, fmt.Errorf("leaf index %d out of range", index)
	}

	level := make([]string, len(leaves))
	for i, leaf := range leaves {
		level[i] = LeafHash(leaf)
	}
	position := index
	proof := []map[string]any{}

	for len(level) > 1 {
		sibling := position ^ 1
		if sibling < len(level) {
			side := "right"
			if sibling < position {
				side = "left"
			}
			proof = append(proof, map[string]any{
				"position": side,
				"hash":     level[sibling],
			})
		}

		next := make([]string, 0, (len(level)+1)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, parentHash(level[i], level[i+1]))
		}
		if len(level)%2 == 1 {
			next = append(next, level[len(level)-1])
		}
		level = next
		position /= 2
	}
	return proof, nil
}

// VerifyMerkleProof verifies an inclusion proof produced by MerkleProof.
func VerifyMerkleProof(root string, leaf any, proof []map[string]any, index int) bool {
	if root == "" || index < 0 {
		return false
	}

	current := LeafHash(leaf)
	for _, step := range proof {
		sibling, ok := step["hash"].(string)
		if !ok {
			return false
		}
		side, ok := step["position"].(string)
		if !ok || (side != "left" && side != "right") {
			return false
		}
		if side == "left" {
			current = parentHash(sibling, current)
		} else {
			current = parentHash(current, sibling)
		}
		index /= 2
	}
	return current == root
}

// ---------------------------------------------------------------------- #
// Ledger state commitment
// ---------------------------------------------------------------------- #

// StateInput gathers everything committed into the state root.
type StateInput struct {
	Balances     map[string]int64
	Nonces       map[string]int64
	Validators   map[string]map[string]any
	TotalSlashed int64
	Storage      map[string]any
	Parameters   map[string]int64
	BaseFee      int64
	TotalBurned  int64
}

// StateEntries returns the ordered, canonical state leaves (accounts,
// contracts, counters).
func StateEntries(input StateInput) []any {
	addresses := map[string]struct{}{}
	for address := range input.Balances {
		addresses[address] = struct{}{}
	}
	for address := range input.Nonces {
		addresses[address] = struct{}{}
	}
	for address := range input.Validators {
		addresses[address] = struct{}{}
	}
	ordered := make([]string, 0, len(addresses))
	for address := range addresses {
		ordered = append(ordered, address)
	}
	sort.Strings(ordered)

	entries := []any{}
	for _, address := range ordered {
		entry := map[string]any{
			"key":     "acct:" + address,
			"address": address,
			"balance": input.Balances[address],
			"nonce":   input.Nonces[address],
		}
		if info := input.Validators[address]; len(info) > 0 {
			entry["validator"] = map[string]any{
				"public_key":     info["public_key"],
				"stake":          toInt64(info["stake"]),
				"joined_height":  toInt64(info["joined_height"]),
				"release_height": info["release_height"],
			}
		}
		entries = append(entries, entry)
	}

	contracts := map[string]any{}
	if input.Storage != nil {
		if raw, ok := input.Storage["contracts"].(map[string]any); ok {
			contracts = raw
		}
	}
	contractIDs := make([]string, 0, len(contracts))
	for id := range contracts {
		contractIDs = append(contractIDs, id)
	}
	sort.Strings(contractIDs)
	for _, id := range contractIDs {
		record := map[string]any{}
		if raw, ok := contracts[id].(map[string]any); ok {
			record = raw
		}
		entries = append(entries,
			map[string]any{"key": "code:" + id, "bytecode": record["bytecode"]},
			map[string]any{"key": "store:" + id, "storage": record["storage"]},
		)
	}

	if input.Storage != nil {
		data := map[string]any{}
		if raw, ok := input.Storage["data"].(map[string]any); ok {
			data = raw
		}
		functions := map[string]any{}
		if raw, ok := input.Storage["functions"].(map[string]any); ok {
			functions = raw
		}
		entries = append(entries,
			map[string]any{"key": "vmdata", "data": data},
			map[string]any{"key": "vmfuncs", "functions": functions},
		)
	}

	entries = append(entries,
		map[string]any{"key": "total_slashed", "total_slashed": input.TotalSlashed},
		map[string]any{"key": "total_burned", "total_burned": input.TotalBurned},
		map[string]any{"key": "base_fee", "base_fee": input.BaseFee},
	)
	if len(input.Parameters) > 0 {
		parameters := map[string]any{}
		for key, value := range input.Parameters {
			parameters[key] = value
		}
		entries = append(entries, map[string]any{
			"key":        "parameters",
			"parameters": parameters,
		})
	}
	return entries
}

// StateRoot is the Merkle root over the state entries.
func StateRoot(input StateInput) string {
	return MerkleRoot(StateEntries(input))
}

// StateEntryProof returns the inclusion proof for one state key, or nil.
func StateEntryProof(input StateInput, key string) map[string]any {
	entries := StateEntries(input)
	for index, entry := range entries {
		record, ok := entry.(map[string]any)
		if !ok || record["key"] != key {
			continue
		}
		proof, _ := MerkleProof(entries, index)
		return map[string]any{
			"key":   key,
			"index": index,
			"leaf":  record,
			"proof": proof,
			"root":  MerkleRoot(entries),
		}
	}
	return nil
}

// AccountKey builds the account state leaf key.
func AccountKey(address string) string {
	return "acct:" + address
}

func toInt64(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	case nil:
		return 0
	default:
		return 0
	}
}
