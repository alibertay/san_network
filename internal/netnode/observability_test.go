package netnode

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
)

// readyTestConfig is a loopback, in-memory config with random ports.
func readyTestConfig(t *testing.T) NodeConfig {
	t.Helper()
	config := DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.ChainID = "san-test-ready"
	config.DBBackend = "memory"
	config.APIPort = 0
	config.P2PPort = 0
	config.PeerPort = 0
	config.ControllerPort = 0
	config.BlockReward = 0
	config.PeerCheckInterval = 3600
	return config
}

func dialable(address string) bool {
	connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

// TestGracefulStopReleasesListeners covers the systemd SIGTERM contract: Stop
// must close the P2P listeners so a restart can bind the same ports.
func TestGracefulStopReleasesListeners(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	node, err := NewNode(readyTestConfig(t), identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	node.mu.Lock()
	listeners := make([]string, 0, len(node.listeners))
	for _, listener := range node.listeners {
		listeners = append(listeners, listener.Addr().String())
	}
	node.mu.Unlock()
	if len(listeners) == 0 {
		t.Fatal("no listeners bound")
	}
	for _, address := range listeners {
		if !dialable(address) {
			t.Fatalf("listener %s is not accepting connections", address)
		}
	}

	node.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		open := false
		for _, address := range listeners {
			if dialable(address) {
				open = true
				break
			}
		}
		if !open {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("listeners still open after Stop: %v", listeners)
}

// TestReadyStateLifecycle walks starting -> ready -> degraded-ish states
// without any network peers. It uses port 0 so the test cannot clash with a
// running devnet.
func TestReadyStateLifecycle(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	config := readyTestConfig(t)

	node, err := NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	state := node.ReadyState()
	if state["state"] != StateStarting {
		t.Fatalf("before Start: state = %v, want starting", state["state"])
	}

	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()

	state = node.ReadyState()
	if state["state"] != StateReady {
		t.Fatalf("after Start: state = %v (checks %v), want ready", state["state"], state["checks"])
	}
	if state["ready"] != true {
		t.Errorf("ready flag = %v", state["ready"])
	}
	checks, _ := state["checks"].(map[string]any)
	for _, name := range []string{"database_loaded", "genesis_verified", "identity_loaded", "p2p_listeners_up"} {
		if checks[name] != true {
			t.Errorf("check %s = %v, want true", name, checks[name])
		}
	}
	if info, _ := state["version_info"].(map[string]any); info == nil || info["protocol_version"] == nil {
		t.Errorf("version_info missing: %v", state["version_info"])
	}
}

// TestReadyStateDegradedWithoutPeers covers the public-devnet requirement:
// with no peer source reachable the node is degraded, never ready.
func TestReadyStateDegradedWithoutPeers(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	config := readyTestConfig(t)
	config.PublicDevnet = true
	config.DiscoveryEnabled = true
	config.PeerRegistryPath = t.TempDir() + "/peers.json"

	node, err := NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer node.Stop()

	state := node.ReadyState()
	if state["state"] != StateDegraded {
		t.Fatalf("state = %v (checks %v), want degraded", state["state"], state["checks"])
	}
	if state["ready"] != false {
		t.Errorf("ready flag = %v", state["ready"])
	}
}
