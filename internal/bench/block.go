package bench

import (
	"fmt"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

// BlockValidation measures full block validation (header, signature checks,
// every transaction signature and execution simulation) for the requested
// transaction counts. Each size gets a fresh in-memory chain; block
// construction is excluded from the timed section.
//
// The benchmark disables block signature and state-root requirements so the
// synthetic blocks are accepted without a validator set; the transaction and
// execution paths are the real ones (node.VerifyBlock -> verifyBlock ->
// simulateBlock).
func BlockValidation(sizes []int) []Result {
	results := make([]Result, 0, len(sizes))
	for _, size := range sizes {
		results = append(results, blockValidation(size))
	}
	return results
}

func blockValidation(count int) Result {
	name := fmt.Sprintf("block_validation.%dtx", count)
	if count < 1 {
		return Result{Name: name, Unit: "blocks/sec", Error: "size must be positive"}
	}

	poolSize := count
	if poolSize > 32 {
		poolSize = 32 // signing 5000 unique keys would dominate the benchmark
	}
	identities := make([]*ledger.NodeIdentity, 0, poolSize)
	allocations := map[string]int64{}
	addresses := make([]string, 0, poolSize)
	for index := 0; index < poolSize; index++ {
		identity, err := ledger.GenerateIdentity()
		if err != nil {
			return Result{Name: name, Unit: "blocks/sec", Error: err.Error()}
		}
		address, err := ledger.AddressFromPublicKey(identity.PublicKeyHex())
		if err != nil {
			return Result{Name: name, Unit: "blocks/sec", Error: err.Error()}
		}
		identities = append(identities, identity)
		addresses = append(addresses, address)
		allocations[address] = int64(1_000_000) * ledger.SANBase // 1M SAN headroom
	}

	config := netnode.DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.ChainID = "san-bench-block"
	config.APIPort = 0
	config.P2PPort = 0
	config.PeerPort = 0
	config.ControllerPort = 0
	config.DBBackend = "memory"
	config.RequireBlockSig = false
	config.RequireStateRoot = false
	config.BlockReward = 0
	config.PeerCheckInterval = 3600
	config.GenesisAllocations = allocations

	node, err := netnode.NewNode(config, identities[0])
	if err != nil {
		return Result{Name: name, Unit: "blocks/sec", Error: err.Error()}
	}
	defer node.Stop()

	tip := node.Tip()
	rate := ledger.FeeRateForTransactionCount(int64(len(tip.Transactions)))
	transactions := make([]any, 0, count)
	nonces := make([]int64, poolSize)
	start := time.Now()
	for index := 0; index < count; index++ {
		slot := index % poolSize
		payload := map[string]any{
			"chain_id": config.ChainID,
			"sender":   identities[slot].PublicKeyHex(),
			"nonce":    nonces[slot],
			"receiver": addresses[(slot+1)%poolSize],
			"value":    int64(1),
		}
		nonces[slot]++
		signature, err := ledger.SignPayload(payload, identities[slot].PrivateKey)
		if err != nil {
			return Result{Name: name, Unit: "blocks/sec", Error: err.Error()}
		}
		payload["signature"] = signature
		transaction, err := ledger.NewTransaction(payload, &rate)
		if err != nil {
			return Result{Name: name, Unit: "blocks/sec", Error: err.Error()}
		}
		transactions = append(transactions, transaction.Payload)
	}
	buildTime := time.Since(start)

	block := ledger.NewBlock(
		tip.Index+1, tip.CurrentBlockHash,
		nil, nil, transactions, nil,
		config.ChainID, nil, 0, nil,
	)
	encoded, _ := canonical.Marshal(block.ToDict())

	start = time.Now()
	valid := node.VerifyBlock(block, true)
	elapsed := time.Since(start)
	if !valid {
		return Result{Name: name, Unit: "blocks/sec", Error: "block failed verification"}
	}

	return Result{
		Name:    name,
		Unit:    "blocks/sec",
		Value:   perSecond(1, elapsed),
		Ops:     1,
		Seconds: elapsed.Seconds(),
		Extra: map[string]any{
			"transactions":     count,
			"tx_per_second":    perSecond(int64(count), elapsed),
			"block_bytes":      len(encoded),
			"build_seconds":    buildTime.Seconds(),
			"signer_pool":      poolSize,
			"average_tx_bytes": len(encoded) / count,
		},
	}
}
