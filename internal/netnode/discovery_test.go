package netnode

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
)

func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// TestDiscoveryFindsPeersAndPropagatesBlock starts two nodes that share only
// the file-backed peer registry (no bootstrap argument) and asserts that they
// find each other and that a block produced by A reaches B.
func TestDiscoveryFindsPeersAndPropagatesBlock(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "peers.json")
	ports := freePorts(t, 8)

	identityA := mustIdentity(t)
	identityB := mustIdentity(t)
	addressA, err := ledger.AddressFromPublicKey(identityA.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey(A): %v", err)
	}
	addressB, err := ledger.AddressFromPublicKey(identityB.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey(B): %v", err)
	}
	allocations := map[string]int64{addressA: 100 * ledger.SANBase}

	newDiscoveryNode := func(offset int, identity *ledger.NodeIdentity) (*Node, error) {
		config := testConfig()
		config.APIPort = ports[offset]
		config.PeerPort = ports[offset+1]
		config.P2PPort = ports[offset+2]
		config.ControllerPort = ports[offset+3]
		config.GenesisAllocations = map[string]int64{addressA: allocations[addressA]}
		config.DiscoveryEnabled = true
		config.PeerRegistryPath = registryPath
		config.DiscoveryInterval = 0.1
		config.DiscoveryTTL = 60
		config.DiscoveryProbe = false
		return NewNode(config, identity)
	}

	nodeA, err := newDiscoveryNode(0, identityA)
	if err != nil {
		t.Fatalf("NewNode(A): %v", err)
	}
	nodeB, err := newDiscoveryNode(4, identityB)
	if err != nil {
		t.Fatalf("NewNode(B): %v", err)
	}

	if err := nodeA.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node A gRPC ports: %v", err)
	}
	defer nodeA.Stop()
	if err := nodeB.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node B gRPC ports: %v", err)
	}
	defer nodeB.Stop()

	waitForCondition(t, 15*time.Second, "both nodes to see each other", func() bool {
		return len(nodeA.Peers()) >= 1 && len(nodeB.Peers()) >= 1
	})

	payload := map[string]any{
		"chain_id": "san-devnet-1",
		"sender":   identityA.PublicKeyHex(),
		"nonce":    int64(0),
		"receiver": addressB,
		"value":    "5",
	}
	signature, err := ledger.SignPayload(payload, identityA.PrivateKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	payload["signature"] = signature
	result, err := nodeA.SubmitTransaction(payload)
	if err != nil {
		t.Fatalf("SubmitTransaction: %v", err)
	}
	if status, _ := result["status"].(string); status != "committed" {
		t.Fatalf("expected committed transaction, got %v", result)
	}

	waitForCondition(t, 15*time.Second, "the transfer to reach B", func() bool {
		account, err := nodeB.GetAccount(addressB)
		if err != nil {
			return false
		}
		return int64Value(account["balance_units"]) >= 5*ledger.SANBase
	})

	account, err := nodeB.GetAccount(addressB)
	if err != nil {
		t.Fatalf("GetAccount(B): %v", err)
	}
	if height := nodeB.Tip().Index; height < 1 {
		t.Fatalf("expected B to have synced at least one block, height=%d", height)
	}
	t.Logf("discovery: A peers=%d, B peers=%d, B balance=%v, B height=%d",
		len(nodeA.Peers()), len(nodeB.Peers()), account["balance"], nodeB.Tip().Index)
}
