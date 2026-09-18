package netnode

import (
	"encoding/binary"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// Consensus test helpers: build deterministic validator sets, sign
// transactions/votes and commit proposals with a chosen proposer.

// consensusConfig is a node config tuned for fast in-process consensus tests:
// no networking-driven production, low stake threshold, test unbonding window.
func consensusConfig() NodeConfig {
	config := testConfig()
	config.DBPath = nil
	config.DBBackend = "memory"
	config.RequireBlockSig = true
	config.RequireStateRoot = true
	config.BlockThresholdFee = 1e9 // tests commit proposals explicitly
	config.ControllerCount = 0
	config.MinValidatorStake = 1
	config.UnbondingPeriod = 3
	config.SlashBps = ledger.DefaultSlashBps
	config.BlockReward = 0
	config.MinBlockIntervalMs = 0
	config.PeerCheckInterval = 3600
	config.SnapshotInterval = 0
	config.PruneKeep = 0
	return config
}

// validatorSet is a node plus `count` funded validator identities.
type validatorSet struct {
	node        *Node
	ids         []*ledger.NodeIdentity
	addrs       []string
	config      NodeConfig
	allocations map[string]int64
}

const testStakeUnits = int64(1000) * ledger.SANBase

func identityAddress(t *testing.T, identity *ledger.NodeIdentity) string {
	t.Helper()
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	return address
}

func validatorSetConfig(t *testing.T, ids []*ledger.NodeIdentity) NodeConfig {
	t.Helper()
	allocations := map[string]int64{}
	for _, identity := range ids {
		allocations[identityAddress(t, identity)] = 10_000 * ledger.SANBase
	}
	config := consensusConfig()
	config.GenesisAllocations = allocations
	return config
}

// newValidatorSet builds a node and deposits/stakes every identity in one
// block, so the whole set is active from height 1.
func newValidatorSet(t *testing.T, count int) *validatorSet {
	t.Helper()
	return newValidatorSetWithKV(t, count, nil)
}

func newValidatorSetWithKV(t *testing.T, count int, kv store.KeyValueStore) *validatorSet {
	t.Helper()
	return newValidatorSetCustom(t, count, kv, nil)
}

// newValidatorSetCustom additionally lets a test adjust the node config
// (block reward, interval, proposer timeout, ...) before construction.
func newValidatorSetCustom(t *testing.T, count int, kv store.KeyValueStore, modify func(*NodeConfig)) *validatorSet {
	t.Helper()
	ids := make([]*ledger.NodeIdentity, count)
	for i := range ids {
		ids[i] = mustIdentity(t)
	}
	return newValidatorSetFromIDs(t, ids, kv, modify)
}

// newValidatorSetFromIDs builds the validator set from fixed keys so a scenario
// can be replayed with identical signatures.
func newValidatorSetFromIDs(t *testing.T, ids []*ledger.NodeIdentity, kv store.KeyValueStore, modify func(*NodeConfig)) *validatorSet {
	t.Helper()
	count := len(ids)
	config := validatorSetConfig(t, ids)
	if modify != nil {
		modify(&config)
	}
	var node *Node
	var err error
	if kv != nil {
		node, err = NewNodeWithStore(config, ids[0], kv)
	} else {
		node, err = NewNode(config, ids[0])
	}
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	addrs := make([]string, count)
	txs := make([]any, 0, count)
	for i, identity := range ids {
		addrs[i] = identityAddress(t, identity)
		txs = append(txs, signTransaction(t, node, identity, map[string]any{
			"chain_id": node.chainID,
			"sender":   identity.PublicKeyHex(),
			"nonce":    int64(0),
			"validator": map[string]any{
				"command": "deposit",
				"amount":  testStakeUnits,
			},
		}).Payload)
	}
	commitProposal(t, node, node.identity, txs)
	return &validatorSet{
		node:        node,
		ids:         ids,
		addrs:       addrs,
		config:      config,
		allocations: config.GenesisAllocations,
	}
}

// signTransaction normalizes, fee-stamps and signs a payload for identity.
func signTransaction(t *testing.T, node *Node, identity *ledger.NodeIdentity, payload map[string]any) *ledger.Transaction {
	t.Helper()
	// The signature is part of the fee basis, so sign first and let
	// NewTransaction compute the fee over the signed payload exactly like the
	// production submission path does.
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	payload["signature"] = signature
	rate := node.blockchain.NextFeeRate()
	tx, err := ledger.NewTransaction(payload, &rate)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}
	return tx
}

func memoizedIdentityAddress(identity *ledger.NodeIdentity) string {
	address, _ := ledger.AddressFromPublicKey(identity.PublicKey)
	return address
}

// proposalRoundFor returns the round in which address is the expected
// proposer, or -1 when the address is not in the active set.
func proposalRoundFor(node *Node, address string) int64 {
	active := node.blockchain.ActiveValidators()
	if len(active) == 0 {
		return 0
	}
	ranked := make([]string, 0, len(active))
	for candidate := range active {
		ranked = append(ranked, candidate)
	}
	sort.Strings(ranked)
	position := sort.SearchStrings(ranked, address)
	if position >= len(ranked) || ranked[position] != address {
		return -1
	}
	height := node.blockchain.Tip().Index + 1
	seed := sha256Sum([]byte(fmt.Sprintf("%s:%d", node.chainID, height)))
	base := int64(binary.BigEndian.Uint64(seed[:8])) % int64(len(ranked))
	return (int64(position) - base + int64(len(ranked))) % int64(len(ranked))
}

// buildProposal builds a signed block on top of the current tip. round < 0
// picks the round in which identity is the expected proposer.
func buildProposal(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any, round int64) *ledger.Block {
	t.Helper()
	if round < 0 {
		round = proposalRoundFor(node, memoizedIdentityAddress(identity))
		if round < 0 {
			t.Fatalf("identity %s is not an active validator", memoizedIdentityAddress(identity))
		}
	}
	tip := node.blockchain.Tip()
	minimum := tip.TimestampFloat() + node.blockchain.MinBlockInterval()
	if round > 0 {
		required := tip.TimestampFloat() + float64(round)*node.blockchain.ProposerTimeout()*0.8
		if required > minimum {
			minimum = required
		}
	}
	timestamp := nowSeconds()
	// Keep timestamps strictly increasing: the median-time rule rejects a
	// block whose timestamp equals the current median.
	if minimum >= timestamp {
		timestamp = minimum + 0.001
	}
	return buildProposalAt(t, node, identity, txs, round, timestamp)
}

// buildProposalAt builds a signed block with an explicit timestamp and a state
// root predicted by simulating the block (so it is valid under the node's
// current rules).
func buildProposalAt(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any, round int64, timestamp float64) *ledger.Block {
	t.Helper()
	tip := node.blockchain.Tip()
	validator := identity.PublicKeyHex()
	provisional := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, validator, nil,
		txs, timestamp, node.chainID, nil, round, "")
	outcome := node.simulateBlock(provisional, false)
	if outcome == nil {
		t.Fatalf("test proposal at height %d failed simulation", tip.Index+1)
	}
	block := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, validator, nil,
		txs, timestamp, node.chainID, stateRootFromOutcome(outcome), round, "")
	block.ValidatorSignature = identity.SignHex([]byte(block.CurrentBlockHash))
	return block
}

// rawProposal builds a signed block without a predicted state root, for tests
// that exercise transaction/command validation directly.
func rawProposal(node *Node, identity *ledger.NodeIdentity, txs []any, round int64) *ledger.Block {
	tip := node.blockchain.Tip()
	block := ledger.NewBlock(tip.Index+1, tip.CurrentBlockHash, identity.PublicKeyHex(), nil,
		txs, nowSeconds(), node.chainID, nil, round, "")
	block.ValidatorSignature = identity.SignHex([]byte(block.CurrentBlockHash))
	return block
}

// governanceApproval signs a set_param approval for a specific transaction.
func governanceApproval(chainID, name string, value, txNonce int64, txSender string, identity *ledger.NodeIdentity) map[string]any {
	message, _ := canonical.Marshal(map[string]any{
		"chain_id":  chainID,
		"command":   "set_param",
		"name":      name,
		"value":     value,
		"tx_sender": txSender,
		"tx_nonce":  txNonce,
	})
	return map[string]any{
		"public_key": identity.PublicKeyHex(),
		"signature":  identity.SignHex(message),
	}
}

// commitProposal builds and commits a block as a historical block.
func commitProposal(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any) *ledger.Block {
	t.Helper()
	block := buildProposal(t, node, identity, txs, -1)
	node.mu.Lock()
	ok := node.commitBlock(block, true)
	node.mu.Unlock()
	if !ok {
		t.Fatalf("commitBlock failed for height %d", block.Index)
	}
	return block
}

// transferPayload builds a signed transfer.
func transferPayload(t *testing.T, node *Node, identity *ledger.NodeIdentity, nonce int64, receiver, amount string) map[string]any {
	t.Helper()
	payload := map[string]any{
		"chain_id": node.chainID,
		"sender":   identity.PublicKeyHex(),
		"nonce":    nonce,
		"receiver": receiver,
		"value":    amount,
	}
	return signTransaction(t, node, identity, payload).Payload
}

// validatorCommandPayload builds a signed staking/slashing command.
func validatorCommandPayload(t *testing.T, node *Node, identity *ledger.NodeIdentity, nonce int64, command map[string]any) map[string]any {
	t.Helper()
	payload := map[string]any{
		"chain_id":  node.chainID,
		"sender":    identity.PublicKeyHex(),
		"nonce":     nonce,
		"validator": command,
	}
	return signTransaction(t, node, identity, payload).Payload
}

// finalityVoteFor builds a signed finality vote.
func finalityVoteFor(t *testing.T, node *Node, identity *ledger.NodeIdentity, height int64, blockHash string) map[string]any {
	t.Helper()
	vote := map[string]any{
		"chain_id":   node.chainID,
		"public_key": identity.PublicKeyHex(),
		"height":     height,
		"block_hash": blockHash,
		"timestamp":  nowSeconds(),
	}
	payload, err := canonical.Marshal(vote)
	if err != nil {
		t.Fatalf("canonical.Marshal(vote): %v", err)
	}
	vote["signature"] = identity.SignHex(payload)
	return vote
}

// ingestBlock feeds a block through the incoming-block path under the lock.
func ingestBlock(t *testing.T, node *Node, block *ledger.Block) bool {
	t.Helper()
	node.mu.Lock()
	accepted := node.processIncomingBlock(block)
	node.mu.Unlock()
	return accepted
}

// branchNode replays the canonical chain up to forkHeight on a fresh node so
// branch blocks can be built against the correct state root.
func branchNode(t *testing.T, base *validatorSet, proposer *ledger.NodeIdentity, forkHeight int64) *Node {
	t.Helper()
	config := consensusConfig()
	config.GenesisAllocations = base.allocations
	node, err := NewNode(config, proposer)
	if err != nil {
		t.Fatalf("branch NewNode: %v", err)
	}
	for _, block := range base.node.blockchain.Chain {
		if block.Index == 0 || block.Index > forkHeight {
			continue
		}
		node.mu.Lock()
		ok := node.commitBlock(block, true)
		node.mu.Unlock()
		if !ok {
			t.Fatalf("branch node could not replay canonical block %d", block.Index)
		}
	}
	return node
}

// reloadNode reopens a node on the same key-value store to exercise startup.
func reloadNode(t *testing.T, vs *validatorSet, kv store.KeyValueStore) (*Node, error) {
	t.Helper()
	return NewNodeWithStore(vs.config, nil, kv)
}

// waitFor is a deterministic test condition poller for the few gRPC-backed
// tests (no consensus test relies on it).
func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
