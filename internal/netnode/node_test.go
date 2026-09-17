package netnode

import (
	"context"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
	"github.com/alibertay/san_network/internal/parity"
)

func testConfig() NodeConfig {
	config := DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.ChainID = "san-devnet-1"
	config.DBBackend = "memory"
	config.RequireBlockSig = true
	config.RequireStateRoot = true
	config.BlockThresholdFee = 0
	config.PeerCheckInterval = 3600
	config.AdvertiseHost = stringPointer("127.0.0.1")
	return config
}

func stringPointer(value string) *string { return &value }

func fixtureChainSection(t *testing.T) map[string]any {
	t.Helper()
	root, err := canonical.Decode(parity.Foundation())
	if err != nil {
		t.Fatalf("cannot decode foundation fixture: %v", err)
	}
	object, ok := root.(map[string]any)
	if !ok {
		t.Fatalf("foundation fixture is not an object")
	}
	chain, ok := object["chain"].(map[string]any)
	if !ok {
		t.Fatalf("foundation fixture has no chain section")
	}
	return chain
}

func mustIdentity(t *testing.T) *ledger.NodeIdentity {
	t.Helper()
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	return identity
}

// 1. Genesis state root and hash must match the Python reference fixture.
func TestGenesisParity(t *testing.T) {
	chain := fixtureChainSection(t)
	config := testConfig()
	config.GenesisAllocations = map[string]int64{
		"0x" + strings.Repeat("aa", 20): 123456789,
		"0x" + strings.Repeat("bb", 20): 0,
	}

	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if got, want := node.blockchain.GenesisStateRoot, chain["genesis_state_root"].(string); got != want {
		t.Errorf("genesis state root: got %s, want %s", got, want)
	}
	if got, want := node.blockchain.Tip().CurrentBlockHash, chain["genesis_hash"].(string); got != want {
		t.Errorf("genesis hash: got %s, want %s", got, want)
	}
	if got := node.CurrentStateRoot(); got != node.blockchain.GenesisStateRoot {
		t.Errorf("current state root at genesis: got %s, want %s", got, node.blockchain.GenesisStateRoot)
	}
}

// 2. A signed transfer commits, updating balances, nonces and the state root.
func TestSubmitTransactionCommitsBlock(t *testing.T) {
	identity := mustIdentity(t)
	senderAddress, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	receiver := "0x" + strings.Repeat("cd", 20)

	config := testConfig()
	config.GenesisAllocations = map[string]int64{senderAddress: 100 * ledger.SANBase}
	config.BlockThresholdFee = 0

	node, err := NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	beforeRoot := node.CurrentStateRoot()

	payload := map[string]any{
		"chain_id": "san-devnet-1",
		"sender":   identity.PublicKeyHex(),
		"nonce":    int64(0),
		"receiver": receiver,
		"value":    "1",
	}
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	payload["signature"] = signature

	result, err := node.SubmitTransaction(payload)
	if err != nil {
		t.Fatalf("SubmitTransaction: %v", err)
	}
	if result["status"] != "committed" {
		t.Fatalf("expected committed transaction, got %v", result)
	}
	if node.blockchain.Tip().Index != 1 {
		t.Fatalf("expected tip height 1, got %d", node.blockchain.Tip().Index)
	}

	account, err := node.GetAccount(senderAddress)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if account["nonce"].(int64) != 1 {
		t.Errorf("sender nonce: got %v, want 1", account["nonce"])
	}
	receiverAccount, err := node.GetAccount(receiver)
	if err != nil {
		t.Fatalf("GetAccount receiver: %v", err)
	}
	if receiverAccount["balance_units"].(int64) != ledger.SANBase {
		t.Errorf("receiver balance: got %v, want %d", receiverAccount["balance_units"], ledger.SANBase)
	}
	if node.CurrentStateRoot() == beforeRoot {
		t.Errorf("state root did not change after the transfer")
	}
}

// 3. Persistence round-trip on a shared memory store.
func TestPersistenceRoundTrip(t *testing.T) {
	identity := mustIdentity(t)
	senderAddress, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	receiver := "0x" + strings.Repeat("ef", 20)

	config := testConfig()
	config.GenesisAllocations = map[string]int64{senderAddress: 1000 * ledger.SANBase}

	kv := store.NewMemoryStore(":memory:")
	first, err := NewNodeWithStore(config, identity, kv)
	if err != nil {
		t.Fatalf("NewNodeWithStore: %v", err)
	}

	payload := map[string]any{
		"chain_id": "san-devnet-1",
		"sender":   identity.PublicKeyHex(),
		"nonce":    int64(0),
		"receiver": receiver,
		"value":    "2",
	}
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	payload["signature"] = signature
	result, err := first.SubmitTransaction(payload)
	if err != nil {
		t.Fatalf("SubmitTransaction: %v", err)
	}
	if result["status"] != "committed" {
		t.Fatalf("expected committed transaction, got %v", result)
	}
	if first.blockchain.Tip().Index != 1 {
		t.Fatalf("expected height 1, got %d", first.blockchain.Tip().Index)
	}

	second, err := NewNodeWithStore(config, nil, kv)
	if err != nil {
		t.Fatalf("second NewNodeWithStore: %v", err)
	}
	if got, want := second.blockchain.Tip().Index, first.blockchain.Tip().Index; got != want {
		t.Errorf("restored height: got %d, want %d", got, want)
	}
	if got, want := second.CurrentStateRoot(), first.CurrentStateRoot(); got != want {
		t.Errorf("restored state root: got %s, want %s", got, want)
	}
	restored, err := second.GetAccount(receiver)
	if err != nil {
		t.Fatalf("GetAccount receiver: %v", err)
	}
	if restored["balance_units"].(int64) != 2*ledger.SANBase {
		t.Errorf("restored receiver balance: got %v", restored["balance_units"])
	}
	restoredSender, err := second.GetAccount(senderAddress)
	if err != nil {
		t.Fatalf("GetAccount sender: %v", err)
	}
	if restoredSender["nonce"].(int64) != 1 {
		t.Errorf("restored sender nonce: got %v, want 1", restoredSender["nonce"])
	}
}

// 4. Lifecycle: the gRPC servers bind and Stop tears everything down.
func TestStartStop(t *testing.T) {
	config := testConfig()
	config.PeerPort = 0
	config.P2PPort = 0
	config.ControllerPort = 0
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	node.Stop()
}

// 5. Self peer records verify and tampered ones do not.
func TestPeerRecords(t *testing.T) {
	node, err := NewNode(testConfig(), mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	record := node.SelfPeerRecord()
	if record["public_key"] == nil || record["signature"] == nil {
		t.Fatalf("self peer record is not signed: %v", record)
	}
	if !node.VerifyPeerRecord(record) {
		t.Fatalf("SelfPeerRecord did not verify")
	}

	tampered := deepCopyStringMap(record)
	tampered["host"] = "10.0.0.9"
	if node.VerifyPeerRecord(tampered) {
		t.Errorf("tampered peer record verified")
	}

	wrongChain := deepCopyStringMap(record)
	wrongChain["chain_id"] = "other-chain"
	if node.VerifyPeerRecord(wrongChain) {
		t.Errorf("peer record for another chain verified")
	}
}
