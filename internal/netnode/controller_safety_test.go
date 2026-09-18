package netnode

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger/store"
)

// publicDevnetConfig returns a valid public-devnet configuration: a peer
// source, a persistent database and the pinned genesis fingerprint the
// profile requires.
func publicDevnetConfig() NodeConfig {
	config := testConfig()
	config.PublicDevnet = true
	config.ControllerCount = 4
	config.BootstrapPeers = []string{"127.0.0.1:1"}
	config.APIHost = "127.0.0.1"
	config.DBBackend = "lmdb"
	dbPath := filepath.Join(os.TempDir(), fmt.Sprintf("san-public-%d.db", os.Getpid()))
	config.DBPath = &dbPath
	config.GenesisFingerprint, _ = GenesisFingerprintFromConfig(config)
	return config
}

// newPublicDevnetNode builds public-devnet nodes on an in-memory store so the
// profile tests do not need the cgo LMDB backend.
func newPublicDevnetNode(t *testing.T, config NodeConfig) (*Node, error) {
	t.Helper()
	return NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:"))
}

func TestPublicDevnetPreflightFailsFast(t *testing.T) {
	t.Run("controller target below the floor", func(t *testing.T) {
		config := publicDevnetConfig()
		config.ControllerCount = 2
		if _, err := newPublicDevnetNode(t, config); err == nil {
			t.Fatalf("node started with a controller target below the public-devnet floor")
		} else if !strings.Contains(err.Error(), "SAN_CONTROLLER_COUNT") {
			t.Fatalf("preflight error is not actionable: %v", err)
		}
	})

	t.Run("explicit floor is honoured", func(t *testing.T) {
		config := publicDevnetConfig()
		config.ControllerCount = 1
		config.ControllerMinCount = 1
		node, err := newPublicDevnetNode(t, config)
		if err != nil {
			t.Fatalf("explicit SAN_CONTROLLER_MIN_COUNT was ignored: %v", err)
		}
		if got := node.Config().ControllerMinCount; got != 1 {
			t.Fatalf("ControllerMinCount: got %d, want 1", got)
		}
	})

	t.Run("no peer source", func(t *testing.T) {
		config := publicDevnetConfig()
		config.BootstrapPeers = nil
		config.Bootstrap = nil
		config.DNSSeeds = nil
		config.DiscoveryEnabled = false
		if _, err := newPublicDevnetNode(t, config); err == nil {
			t.Fatalf("node started without any way to discover peers")
		} else if !strings.Contains(err.Error(), "peer source") {
			t.Fatalf("preflight error is not actionable: %v", err)
		}
	})

	t.Run("dev mode is unaffected", func(t *testing.T) {
		config := testConfig()
		config.ControllerCount = 0
		if _, err := NewNode(config, mustIdentity(t)); err != nil {
			t.Fatalf("dev/bootstrap mode must keep the permissive behavior: %v", err)
		}
	})
}

func TestControllerMetricsTargetVsEffective(t *testing.T) {
	config := publicDevnetConfig()
	config.ControllerCount = 7
	node, err := newPublicDevnetNode(t, config)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	snapshot := node.MetricsSnapshot()
	if got := snapshot["controllers_target"]; got != int64(7) {
		t.Fatalf("controllers_target: got %v, want 7", got)
	}
	if got := snapshot["controllers"]; got != int64(0) {
		t.Fatalf("controllers: got %v, want 0", got)
	}
	if got := snapshot["peer_bans_active"]; got != int64(0) {
		t.Fatalf("peer_bans_active: got %v, want 0", got)
	}
}

func TestPublicDevnetEmitsEmptyControllerWarning(t *testing.T) {
	config := publicDevnetConfig()
	config.PeerCheckInterval = 3600
	node, err := newPublicDevnetNode(t, config)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	var buffer bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()

	if err := node.Start(context.Background()); err != nil {
		t.Skipf("cannot bind gRPC ports: %v", err)
	}
	defer node.Stop()
	log.SetOutput(previousWriter)

	output := buffer.String()
	if !strings.Contains(output, "WARNING") || !strings.Contains(output, "controller set is empty") {
		t.Fatalf("startup did not warn about the empty controller set:\n%s", output)
	}
}

func TestStaleControllerRecordIsDropped(t *testing.T) {
	local := peerTestNode(t, 18201)
	remote := peerTestNode(t, 18202)

	fresh := remote.SelfPeerRecord()
	if added := local.AddPeer(fresh); added != 1 {
		t.Fatalf("fresh controller record was not added")
	}

	// Simulate a record that was admitted while fresh but stopped being
	// re-announced: the selection must drop it once its timestamp ages out.
	stale := remote.SelfPeerRecord()
	stale["api_port"] = int64(18203)
	stale["timestamp"] = nowSeconds() - local.config.PeerRecordTTL - 30
	resignRecord(t, remote, stale)

	local.mu.Lock()
	local.PEERS = append(local.PEERS, stale)
	local.mu.Unlock()
	controllers := local.selectControllers()
	for _, controller := range controllers {
		if SamePeer(controller, stale) {
			t.Fatalf("stale controller record was selected: %v", controller)
		}
	}

	// Deduplication still applies to the fresh set.
	local.mu.Lock()
	local.PEERS = append(local.PEERS, deepCopyStringMap(fresh))
	local.mu.Unlock()
	controllers = dedupeControllers(local.selectControllers())
	if len(controllers) != 1 {
		t.Fatalf("duplicate controller records were not deduplicated: %d", len(controllers))
	}
}
