package netnode

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/genesis"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// writeTestGenesis renders a valid genesis file with a chain id of choice and
// returns its path plus the resolved spec.
func writeTestGenesis(t *testing.T, chainID string) (string, *genesis.Spec) {
	t.Helper()
	spec := &genesis.Spec{
		FormatVersion: genesis.FormatVersion,
		ChainID:       chainID,
		Allocations:   map[string]string{},
		Parameters: genesis.Parameters{
			BlockReward:        "2",
			MinBlockIntervalMs: 1000,
			ProposerTimeoutMs:  6000,
			BlockGasLimit:      30_000_000,
			UnbondingPeriod:    100,
			SlashBps:           5000,
			MinValidatorStake:  "0",
		},
		Validators: []string{},
	}
	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("genesis.Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := genesis.Load(path)
	if err != nil {
		t.Fatalf("genesis.Load: %v", err)
	}
	return path, loaded
}

// TestGenesisFingerprintMismatchFailsStartup pins that a node with a pinned
// fingerprint for a different network refuses to start.
func TestGenesisFingerprintMismatchFailsStartup(t *testing.T) {
	config := testConfig()
	config.GenesisFingerprint = strings.Repeat("ab", 32)
	if _, err := NewNode(config, mustIdentity(t)); err == nil {
		t.Fatalf("node started with a mismatched genesis fingerprint")
	} else if !strings.Contains(err.Error(), "genesis fingerprint mismatch") {
		t.Fatalf("startup error is not actionable: %v", err)
	}
}

// TestGenesisFingerprintMatchAccepted pins that a pinned fingerprint computed
// from the same configuration starts and is exposed.
func TestGenesisFingerprintMatchAccepted(t *testing.T) {
	config := testConfig()
	fingerprint, err := GenesisFingerprintFromConfig(config)
	if err != nil {
		t.Fatalf("GenesisFingerprintFromConfig: %v", err)
	}
	config.GenesisFingerprint = fingerprint
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if node.GenesisFingerprint() != fingerprint {
		t.Fatalf("GenesisFingerprint: %s vs %s", node.GenesisFingerprint(), fingerprint)
	}
}

// TestGenesisFileAppliesAndBinds covers the --genesis-file/ SAN_GENESIS_FILE
// path: the file is authoritative and its fingerprint becomes the node's.
func TestGenesisFileAppliesAndBinds(t *testing.T) {
	path, spec := writeTestGenesis(t, "san-file-test-1")
	config := testConfig()
	config.ChainID = "san-file-test-1"
	config.GenesisFile = path
	config.GenesisFingerprint = ""

	node, err := NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:"))
	if err != nil {
		t.Fatalf("NewNodeWithStore: %v", err)
	}
	if node.ChainID() != spec.ChainID {
		t.Fatalf("chain id: %s vs %s", node.ChainID(), spec.ChainID)
	}
	if node.GenesisFingerprint() != spec.Fingerprint {
		t.Fatalf("fingerprint: %s vs %s", node.GenesisFingerprint(), spec.Fingerprint)
	}
	applied := node.Config()
	if applied.BlockReward != 2 || applied.MinBlockIntervalMs != 1000 || applied.ProposerTimeout != 6 {
		t.Fatalf("genesis parameters were not applied: %+v", applied)
	}
	if applied.UnbondingPeriod != 100 || applied.SlashBps != 5000 || applied.BlockGasLimit != 30_000_000 {
		t.Fatalf("genesis parameters were not applied: %+v", applied)
	}
}

// TestGenesisFileChainIDConflictRejected pins that a chain id that contradicts
// the file is a startup error, not a silent override.
func TestGenesisFileChainIDConflictRejected(t *testing.T) {
	path, _ := writeTestGenesis(t, "san-file-test-1")
	t.Setenv("SAN_CHAIN_ID", "san-other-1")
	config := testConfig()
	config.GenesisFile = path
	if _, err := NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:")); err == nil {
		t.Fatalf("conflicting SAN_CHAIN_ID was accepted")
	} else if !strings.Contains(err.Error(), "chain id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestGenesisFileFingerprintConflictRejected pins the same for a pinned
// fingerprint.
func TestGenesisFileFingerprintConflictRejected(t *testing.T) {
	path, _ := writeTestGenesis(t, "san-file-test-1")
	t.Setenv("SAN_GENESIS_FINGERPRINT", strings.Repeat("cd", 32))
	config := testConfig()
	config.GenesisFile = path
	if _, err := NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:")); err == nil {
		t.Fatalf("conflicting SAN_GENESIS_FINGERPRINT was accepted")
	} else if !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestGenesisFileMissingRejected covers a missing file.
func TestGenesisFileMissingRejected(t *testing.T) {
	config := testConfig()
	config.GenesisFile = filepath.Join(t.TempDir(), "nope.json")
	if _, err := NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:")); err == nil {
		t.Fatalf("missing genesis file was accepted")
	}
}

// TestHelloAcceptRejectMatrix pins every HELLO rejection reason and its metric.
func TestHelloAcceptRejectMatrix(t *testing.T) {
	node := newDispatchNode(t)
	valid := node.HelloPayload("HELLO")
	if !node.VerifyHello(valid) {
		t.Fatalf("self-authored HELLO did not verify")
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"missing genesis", func(record map[string]any) { delete(record, "genesis") }, "genesis"},
		{"wrong genesis", func(record map[string]any) { record["genesis"] = strings.Repeat("ab", 32) }, "genesis"},
		{"missing software", func(record map[string]any) { delete(record, "software") }, "software"},
		{"missing capability", func(record map[string]any) { record["capabilities"] = []any{"something-else"} }, "capability"},
		{"wrong protocol", func(record map[string]any) { record["protocol"] = int64(ProtocolVersion + 1) }, "protocol"},
		{"wrong chain", func(record map[string]any) { record["chain_id"] = "other-chain" }, "chain"},
		{"stale", func(record map[string]any) { record["timestamp"] = nowSeconds() - HelloTTL - 30 }, "stale"},
		{"bad signature", func(record map[string]any) { record["signature"] = "00" }, "signature"},
		{"malformed timestamp", func(record map[string]any) { delete(record, "timestamp") }, "malformed"},
		{"wrong type", func(record map[string]any) { record["type"] = "NOPE" }, "type"},
	}
	for _, testCase := range cases {
		record := deepCopyStringMap(valid)
		testCase.mutate(record)
		before := metricValue(node, "handshake_rejected_"+testCase.reason)
		if node.VerifyHello(record) {
			t.Errorf("%s: hostile HELLO was accepted", testCase.name)
			continue
		}
		if got := metricValue(node, "handshake_rejected_"+testCase.reason); got != before+1 {
			t.Errorf("%s: reason metric %s = %d, want %d", testCase.name, testCase.reason, got, before+1)
		}
	}
}

// TestLegacyHandshakeWindow pins the explicit protocol-2 compatibility mode:
// strict nodes reject v2, legacy nodes accept it and emit v2 themselves.
func TestLegacyHandshakeWindow(t *testing.T) {
	strict := newDispatchNode(t)
	legacy := newDispatchNode(t)
	legacy.config.AllowLegacyHandshake = true

	record := map[string]any{
		"type":       "HELLO",
		"protocol":   int64(LegacyProtocolVersion),
		"chain_id":   legacy.chainID,
		"public_key": legacy.GetPublicKey(),
		"timestamp":  nowSeconds(),
	}
	record["signature"] = legacy.identity.SignHex(mustCanonical(t, mapWithout(record, helloMetaFields)))

	if !legacy.VerifyHello(record) {
		t.Fatalf("legacy node rejected a protocol-2 HELLO")
	}
	if strict.VerifyHello(record) {
		t.Fatalf("strict node accepted a protocol-2 HELLO")
	}
	if payload := legacy.HelloPayload("HELLO"); payload["protocol"] != int64(LegacyProtocolVersion) {
		t.Fatalf("legacy outgoing protocol: %v", payload["protocol"])
	} else if _, present := payload["genesis"]; present {
		t.Fatalf("legacy outgoing HELLO carries protocol-3 fields")
	}

	unsupported := deepCopyStringMap(record)
	unsupported["protocol"] = int64(LegacyProtocolVersion + 2)
	unsupported["signature"] = legacy.identity.SignHex(mustCanonical(t, mapWithout(unsupported, helloMetaFields)))
	if legacy.VerifyHello(unsupported) {
		t.Fatalf("legacy node accepted an unsupported protocol version")
	}
}

// TestPeerRecordGenesisBinding pins that peer records carry the fingerprint
// and that mismatches/missing bindings are rejected in strict mode.
func TestPeerRecordGenesisBinding(t *testing.T) {
	local := newDispatchNode(t)
	remote := newDispatchNode(t)

	fresh := remote.SelfPeerRecord()
	if fresh["genesis"] != remote.genesisFingerprint {
		t.Fatalf("self record does not carry the genesis fingerprint")
	}
	if !local.VerifyPeerRecord(fresh) {
		t.Fatalf("same-genesis peer record was rejected")
	}

	mismatch := deepCopyStringMap(fresh)
	mismatch["genesis"] = strings.Repeat("ab", 32)
	if local.VerifyPeerRecord(resignRecord(t, remote, mismatch)) {
		t.Fatalf("different-genesis peer record was accepted")
	}

	missing := deepCopyStringMap(fresh)
	delete(missing, "genesis")
	resignedMissing := resignRecord(t, remote, missing)
	if local.VerifyPeerRecord(resignedMissing) {
		t.Fatalf("genesis-less peer record was accepted in strict mode")
	}
	local.config.AllowLegacyHandshake = true
	if !local.VerifyPeerRecord(resignedMissing) {
		t.Fatalf("genesis-less peer record was rejected in legacy mode")
	}
}

// TestPeerStatusAndSyncCarryGenesis pins the exported chain identity.
func TestPeerStatusAndSyncCarryGenesis(t *testing.T) {
	node := newDispatchNode(t)
	if got := node.PeerStatus()["genesis"]; got != node.genesisFingerprint {
		t.Fatalf("PeerStatus genesis: %v", got)
	}
	if got := node.GetSyncPayload(0, 1)["genesis"]; got != node.genesisFingerprint {
		t.Fatalf("sync payload genesis: %v", got)
	}
}

// TestPublicDevnetProfileValidationMatrix covers the secure public-devnet
// profile and its explicit override.
func TestPublicDevnetProfileValidationMatrix(t *testing.T) {
	base := publicDevnetConfig()
	unsafe := []struct {
		name   string
		mutate func(*NodeConfig)
	}{
		{"zero controllers", func(config *NodeConfig) { config.ControllerCount = 0 }},
		{"no peer source", func(config *NodeConfig) {
			config.BootstrapPeers = nil
			config.Bootstrap = nil
			config.DNSSeeds = nil
			config.DiscoveryEnabled = false
		}},
		{"memory backend", func(config *NodeConfig) { config.DBBackend = "memory" }},
		{"empty db path", func(config *NodeConfig) { config.DBPath = nil }},
		{"unauthenticated public API", func(config *NodeConfig) {
			config.APIHost = "0.0.0.0"
			config.APIToken = ""
		}},
		{"unauthenticated faucet", func(config *NodeConfig) {
			config.FaucetEnabled = true
			config.APIHost = "127.0.0.1"
			config.APIToken = ""
		}},
		{"legacy handshake", func(config *NodeConfig) { config.AllowLegacyHandshake = true }},
		{"missing genesis fingerprint", func(config *NodeConfig) { config.GenesisFingerprint = "" }},
	}
	for _, testCase := range unsafe {
		config := base
		testCase.mutate(&config)
		if err := config.Validate(); err == nil {
			t.Errorf("%s: insecure public-devnet config passed validation", testCase.name)
		}
		config.AllowInsecurePublic = true
		if err := config.Validate(); err != nil {
			t.Errorf("%s: explicit override did not downgrade the error: %v", testCase.name, err)
		}
	}

	safeLoopback := base
	safeLoopback.APIHost = "127.0.0.1"
	safeLoopback.APIToken = ""
	if err := safeLoopback.Validate(); err != nil {
		t.Errorf("loopback-only API without a token must be accepted: %v", err)
	}
	safeFaucet := base
	safeFaucet.FaucetEnabled = true
	safeFaucet.APIToken = "secret-token"
	safeFaucet.APIHost = "0.0.0.0"
	if err := safeFaucet.Validate(); err != nil {
		t.Errorf("authenticated faucet must be accepted: %v", err)
	}
}

// TestStartupLogsGenesis pins that the genesis identity is printed before the
// node accepts remote data.
func TestStartupLogsGenesis(t *testing.T) {
	config := readyTestConfig(t)
	node, err := NewNodeWithStore(config, mustIdentity(t), store.NewMemoryStore(":memory:"))
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
	if !strings.Contains(output, "GENESIS chain_id="+config.ChainID) ||
		!strings.Contains(output, node.GenesisFingerprint()) {
		t.Fatalf("startup did not print the genesis identity:\n%s", output)
	}
}
