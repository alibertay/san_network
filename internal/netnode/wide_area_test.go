package netnode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
)

// TestWideAreaBootstrapCacheAndRestart is the cross-machine-style integration
// test: node A runs with the local registry DISABLED, node B joins only
// through SAN_BOOTSTRAP (the wide-area path), learns A's advertised address
// over the Bootstrap RPC, syncs a transfer, saves its address cache, restarts
// without any seed and reconnects from the cache.
func TestWideAreaBootstrapCacheAndRestart(t *testing.T) {
	cacheDir := t.TempDir()
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

	newNodeA := func() *Node {
		config := testConfig()
		config.APIPort = ports[0]
		config.PeerPort = ports[1]
		config.P2PPort = ports[2]
		config.ControllerPort = ports[3]
		config.GenesisAllocations = allocations
		config.ControllerCount = 0
		config.DiscoveryEnabled = false
		config.Bootstrap = nil
		config.BootstrapPeers = nil
		config.DNSSeeds = nil
		config.PeerCachePath = filepath.Join(t.TempDir(), "a-peers.json")
		node, err := NewNode(config, identityA)
		if err != nil {
			t.Fatalf("NewNode(A): %v", err)
		}
		return node
	}
	newNodeB := func(bootstrap []string, cachePath string) *Node {
		config := testConfig()
		config.APIPort = ports[4]
		config.PeerPort = ports[5]
		config.P2PPort = ports[6]
		config.ControllerPort = ports[7]
		config.GenesisAllocations = allocations
		config.ControllerCount = 0
		config.DiscoveryEnabled = false
		config.Bootstrap = nil
		config.BootstrapPeers = bootstrap
		config.DNSSeeds = nil
		config.PeerCachePath = cachePath
		config.DiscoveryInterval = 0.2
		config.OutboundPeers = 4
		node, err := NewNode(config, identityB)
		if err != nil {
			t.Fatalf("NewNode(B): %v", err)
		}
		return node
	}

	nodeA := newNodeA()
	if err := nodeA.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node A gRPC ports: %v", err)
	}
	defer nodeA.Stop()
	if nodeA.addrman != nil {
		t.Fatalf("node A must run without discovery (registry disabled, no seeds)")
	}

	cachePath := filepath.Join(cacheDir, "b-peers.json")
	bootstrap := fmt.Sprintf("127.0.0.1:%d", ports[1])
	nodeB := newNodeB([]string{bootstrap}, cachePath)
	if err := nodeB.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node B gRPC ports: %v", err)
	}

	waitForCondition(t, 15*time.Second, "B to learn A's advertised address over bootstrap", func() bool {
		for _, raw := range nodeB.Peers() {
			peer, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(peer["host"]) == "127.0.0.1" &&
				int64Value(peer["peer_port"]) == int64(ports[1]) &&
				int64Value(peer["p2p_port"]) == int64(ports[2]) {
				return true
			}
		}
		return false
	})
	if !nodeB.addrman.Has(map[string]any{"host": "127.0.0.1", "peer_port": int64(ports[1])}) {
		t.Fatalf("A's address is not in B's address manager: %v", nodeB.addrman.Snapshot())
	}
	t.Logf("phase 1: B peers=%v", nodeB.Peers())

	submitTransfer(t, nodeA, identityA, addressB, "5")
	waitForBalance(t, nodeB, addressB, 5*ledger.SANBase)

	nodeB.Stop()
	if info, err := os.Stat(cachePath); err != nil || info.Size() == 0 {
		t.Fatalf("B did not persist its address cache: %v", err)
	}
	t.Logf("phase 2: cache persisted at %s (%d bytes)", cachePath, fileSize(t, cachePath))

	// Restart B with no seed at all: only the persisted cache can reconnect it.
	nodeB2 := newNodeB(nil, cachePath)
	if !nodeB2.discoveryActive() {
		t.Fatalf("a node with a peer cache must activate wide-area discovery")
	}
	if err := nodeB2.Start(context.Background()); err != nil {
		t.Skipf("cannot bind node B2 gRPC ports: %v", err)
	}
	defer nodeB2.Stop()

	waitForCondition(t, 15*time.Second, "B2 to reconnect from the peer cache", func() bool {
		return len(nodeB2.Peers()) >= 1
	})
	t.Logf("phase 3: B2 reconnected, peers=%v", nodeB2.Peers())

	submitTransfer(t, nodeA, identityA, addressB, "5")
	waitForBalance(t, nodeB2, addressB, 10*ledger.SANBase)
	t.Logf("phase 4: transfer propagated after restart at height=%d", nodeB2.Tip().Index)
}

// submitTransfer signs and submits a transfer from the node identity.
func submitTransfer(t *testing.T, node *Node, identity *ledger.NodeIdentity, receiver, amount string) {
	t.Helper()
	sender, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	account, err := node.GetAccount(sender)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	nonce := int64Value(account["nonce"])
	payload := map[string]any{
		"chain_id": "san-devnet-1",
		"sender":   identity.PublicKeyHex(),
		"nonce":    nonce,
		"receiver": receiver,
		"value":    amount,
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
	if status, _ := result["status"].(string); status != "committed" {
		t.Fatalf("transfer not committed: %v", result)
	}
}

// waitForBalance polls the balance while nudging the node to sync.
func waitForBalance(t *testing.T, node *Node, address string, minimum int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := int64(0)
	for time.Now().Before(deadline) {
		node.requestSync(context.Background())
		account, err := node.GetAccount(address)
		if err == nil {
			last = int64Value(account["balance_units"])
			if last >= minimum {
				t.Logf("balance reached %d units (height=%d)", last, node.Tip().Index)
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("balance did not reach %d units (last %d)", minimum, last)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
