package bench

import (
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

// Sizes measures the real post-quantum wire sizes used by the bandwidth model
// in docs/pq-bandwidth.md. Every payload is signed with a fresh ML-DSA-44 key
// and encoded with the canonical encoder, so the byte counts match what the
// node actually gossips.
func Sizes() []Result {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		return []Result{{Name: "size.transfer_tx", Unit: "bytes", Error: err.Error()}}
	}
	address, err := ledger.AddressFromPublicKey(identity.PublicKeyHex())
	if err != nil {
		return []Result{{Name: "size.transfer_tx", Unit: "bytes", Error: err.Error()}}
	}
	chainID := ledger.DefaultChainID
	rate := ledger.MinFeePerByte
	results := []Result{}

	// Plain SAN transfer.
	transfer := map[string]any{
		"chain_id": chainID,
		"sender":   identity.PublicKeyHex(),
		"nonce":    int64(0),
		"receiver": address,
		"value":    int64(1),
	}
	results = append(results, measureSigned("size.transfer_tx", transfer, identity, rate))

	// Contract deploy (small PENA budget; the bytecode is a few hundred bytes).
	deploy := map[string]any{
		"chain_id":  chainID,
		"sender":    identity.PublicKeyHex(),
		"nonce":     int64(1),
		"gas_limit": int64(2_000_000),
		"gas_price": int64(1),
		"contract_code": map[string]any{
			"command":     "deploy",
			"contract_id": "sizes",
			"pena_code":   "x := 0\nfunction set(v) { x = v }\nfunction get() { return x }\n",
		},
	}
	results = append(results, measureSigned("size.contract_deploy_tx", deploy, identity, rate))

	// Contract call.
	call := map[string]any{
		"chain_id":  chainID,
		"sender":    identity.PublicKeyHex(),
		"nonce":     int64(2),
		"gas_limit": int64(1_000_000),
		"gas_price": int64(1),
		"contract_code": map[string]any{
			"command":       "run",
			"contract_id":   "sizes",
			"function_name": "set",
			"params":        []any{int64(42)},
		},
	}
	results = append(results, measureSigned("size.contract_call_tx", call, identity, rate))

	// Finality vote (the shape maybeVote gossips).
	vote := map[string]any{
		"chain_id":   chainID,
		"public_key": identity.PublicKeyHex(),
		"height":     int64(12345),
		"block_hash": address,
		"timestamp":  float64(time.Now().Unix()),
	}
	results = append(results, measureSigned("size.finality_vote", vote, identity, 0))

	// Controller approval (BLOCK_VOTE_RESPONSE).
	controller := map[string]any{
		"type":       "BLOCK_VOTE_RESPONSE",
		"chain_id":   chainID,
		"approved":   true,
		"block_hash": address,
		"public_key": identity.PublicKeyHex(),
	}
	results = append(results, measureSigned("size.controller_vote", controller, identity, 0))

	// Signed peer record (SelfPeerRecord shape).
	record := map[string]any{
		"host":            "203.0.113.10",
		"api_port":        int64(8000),
		"p2p_port":        int64(8765),
		"peer_port":       int64(8770),
		"controller_port": int64(8769),
		"chain_id":        chainID,
		"public_key":      identity.PublicKeyHex(),
		"timestamp":       float64(time.Now().Unix()),
		"protocol":        int64(netnode.ProtocolVersion),
	}
	results = append(results, measureSigned("size.peer_record", record, identity, 0))

	// Blocks with N transactions (first N transfer payloads with distinct
	// nonces, signed the same way the node signs).
	blockSizes := []int{1, 10, 100, 1000}
	transactions := []any{}
	for count := 0; count < 1000; count++ {
		payload := map[string]any{
			"chain_id": chainID,
			"sender":   identity.PublicKeyHex(),
			"nonce":    int64(count),
			"receiver": address,
			"value":    int64(1),
		}
		signature, err := ledger.SignPayload(payload, identity.PrivateKey)
		if err != nil {
			continue
		}
		payload["signature"] = signature
		transaction, err := ledger.NewTransaction(payload, &rate)
		if err != nil {
			continue
		}
		transactions = append(transactions, transaction.Payload)
	}
	for _, count := range blockSizes {
		if count > len(transactions) {
			continue
		}
		block := ledger.NewBlock(1, "0", nil, nil, transactions[:count], nil, chainID, nil, 0, nil)
		encoded, err := canonical.Marshal(block.ToDict())
		if err != nil {
			results = append(results, Result{Name: blockSizeName(count), Unit: "bytes", Error: err.Error()})
			continue
		}
		results = append(results, Result{
			Name:  blockSizeName(count),
			Unit:  "bytes",
			Value: float64(len(encoded)),
			Bytes: int64(len(encoded)),
			Extra: map[string]any{"transactions": count, "average_tx_bytes": len(encoded) / count},
		})
	}
	return results
}

func blockSizeName(count int) string {
	switch count {
	case 1:
		return "size.block_1tx"
	case 10:
		return "size.block_10tx"
	case 100:
		return "size.block_100tx"
	case 1000:
		return "size.block_1000tx"
	default:
		return "size.block"
	}
}

func measureSigned(name string, payload map[string]any, identity *ledger.NodeIdentity, rate int64) Result {
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		return Result{Name: name, Unit: "bytes", Error: err.Error()}
	}
	payload["signature"] = signature
	if rate > 0 {
		transaction, err := ledger.NewTransaction(payload, &rate)
		if err != nil {
			return Result{Name: name, Unit: "bytes", Error: err.Error()}
		}
		payload = transaction.Payload
	}
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return Result{Name: name, Unit: "bytes", Error: err.Error()}
	}
	return Result{Name: name, Unit: "bytes", Value: float64(len(encoded)), Bytes: int64(len(encoded))}
}
