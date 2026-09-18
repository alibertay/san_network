package genesis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
)

func testAddress(t *testing.T) string {
	t.Helper()
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	return address
}

func validSpec(t *testing.T) *Spec {
	t.Helper()
	spec := &Spec{
		FormatVersion: FormatVersion,
		ChainID:       "san-test-1",
		Allocations: map[string]string{
			testAddress(t): "10000",
		},
		Parameters: Parameters{
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
	fingerprint, err := spec.ComputeFingerprint()
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	spec.Fingerprint = fingerprint
	return spec
}

// TestRoundTripParity pins that marshalling and re-parsing reproduce the same
// fingerprint and values (the file replacement path).
func TestRoundTripParity(t *testing.T) {
	spec := validSpec(t)
	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.ChainID != spec.ChainID || parsed.Fingerprint != spec.Fingerprint {
		t.Fatalf("round trip changed the chain: %+v vs %+v", parsed, spec)
	}
	if parsed.Parameters != spec.Parameters {
		t.Fatalf("round trip changed parameters: %+v vs %+v", parsed.Parameters, spec.Parameters)
	}
	for address, amount := range spec.Allocations {
		if parsed.Allocations[address] != amount {
			t.Fatalf("round trip changed allocation %s: %q vs %q", address, parsed.Allocations[address], amount)
		}
	}
	// Stable across repeated marshalling.
	again, err := parsed.Marshal()
	if err != nil {
		t.Fatalf("Marshal again: %v", err)
	}
	if string(again) != string(data) {
		t.Fatalf("marshalling is not stable:\n%s\n%s", data, again)
	}
}

// TestFingerprintStabilityAndSensitivity pins that the fingerprint depends on
// every genesis field.
func TestFingerprintStabilityAndSensitivity(t *testing.T) {
	spec := validSpec(t)
	baseline := spec.Fingerprint
	repeated, err := spec.ComputeFingerprint()
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	if repeated != baseline {
		t.Fatalf("fingerprint is not stable: %s vs %s", repeated, baseline)
	}

	mutations := map[string]func(*Spec){
		"chain id":         func(spec *Spec) { spec.ChainID = "san-other-1" },
		"allocation":       func(spec *Spec) { spec.Allocations[testAddress(t)] = "1" },
		"block reward":     func(spec *Spec) { spec.Parameters.BlockReward = "3" },
		"block interval":   func(spec *Spec) { spec.Parameters.MinBlockIntervalMs = 2000 },
		"proposer timeout": func(spec *Spec) { spec.Parameters.ProposerTimeoutMs = 7000 },
		"gas limit":        func(spec *Spec) { spec.Parameters.BlockGasLimit = 31_000_000 },
		"unbonding":        func(spec *Spec) { spec.Parameters.UnbondingPeriod = 101 },
		"slash bps":        func(spec *Spec) { spec.Parameters.SlashBps = 5001 },
		"min stake":        func(spec *Spec) { spec.Parameters.MinValidatorStake = "1" },
	}
	for name, mutate := range mutations {
		changed := validSpec(t)
		mutate(changed)
		fingerprint, err := changed.ComputeFingerprint()
		if err != nil {
			t.Fatalf("%s: ComputeFingerprint: %v", name, err)
		}
		if fingerprint == baseline {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}

	validators := validSpec(t)
	validators.Validators = []string{testAddress(t)}
	fingerprint, err := validators.ComputeFingerprint()
	if err != nil {
		t.Fatalf("validators: ComputeFingerprint: %v", err)
	}
	if fingerprint == baseline {
		t.Errorf("validator bootstrap list did not change the fingerprint")
	}
}

// TestFingerprintMismatchRejected covers a hand-edited file whose declared
// fingerprint no longer matches its contents.
func TestFingerprintMismatchRejected(t *testing.T) {
	spec := validSpec(t)
	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tampered := strings.Replace(string(data), `"block_reward": "2"`, `"block_reward": "99"`, 1)
	if _, err := Parse([]byte(tampered)); err == nil {
		t.Fatalf("tampered genesis file was accepted")
	} else if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	// A declared-but-wrong fingerprint is rejected without any other change.
	declared := strings.Replace(string(data), spec.Fingerprint, strings.Repeat("ab", 32), 1)
	if _, err := Parse([]byte(declared)); err == nil {
		t.Fatalf("stale declared fingerprint was accepted")
	} else if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMissingAndInvalidFiles covers the file handling paths.
func TestMissingAndInvalidFiles(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatalf("missing file was accepted")
	}

	invalid := []string{
		"",
		"{",
		`{"format_version":2,"chain_id":"x"}`,
		`{"format_version":1}`,
		`{"format_version":1,"chain_id":"x","allocations":{},"parameters":{},"validators":[]}`,
		`{"format_version":1,"chain_id":"x","allocations":{},"parameters":{"block_reward":"2","min_block_interval_ms":1000,"proposer_timeout_ms":6000,"block_gas_limit":30000000,"unbonding_period":1,"slash_bps":5000,"min_validator_stake":"0"},"validators":[],"unknown_field":1}`,
		`{"format_version":1,"chain_id":"x","allocations":{"nope":"1"},"parameters":{"block_reward":"2","min_block_interval_ms":1000,"proposer_timeout_ms":6000,"block_gas_limit":30000000,"unbonding_period":1,"slash_bps":5000,"min_validator_stake":"0"},"validators":[]}`,
		`{"format_version":1,"chain_id":"x","allocations":{"0x0000000000000000000000000000000000000000":"-1"},"parameters":{"block_reward":"2","min_block_interval_ms":1000,"proposer_timeout_ms":6000,"block_gas_limit":30000000,"unbonding_period":1,"slash_bps":5000,"min_validator_stake":"0"},"validators":[]}`,
		`{"format_version":1,"chain_id":"x","allocations":{},"parameters":{"block_reward":"2","min_block_interval_ms":1000,"proposer_timeout_ms":6000,"block_gas_limit":30000000,"unbonding_period":1,"slash_bps":10001,"min_validator_stake":"0"},"validators":[]}`,
		`{"format_version":1,"chain_id":"x","allocations":{},"parameters":{"block_reward":"2","min_block_interval_ms":1000,"proposer_timeout_ms":6000,"block_gas_limit":30000000,"unbonding_period":1,"slash_bps":5000,"min_validator_stake":"0"},"validators":[]} trailing`,
	}
	for index, document := range invalid {
		if _, err := Parse([]byte(document)); err == nil {
			t.Errorf("invalid document %d was accepted: %s", index, document)
		}
	}
}

// TestLoadValidFile covers the on-disk path.
func TestLoadValidFile(t *testing.T) {
	spec := validSpec(t)
	data, err := spec.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Fingerprint != spec.Fingerprint {
		t.Fatalf("fingerprint changed on load: %s vs %s", loaded.Fingerprint, spec.Fingerprint)
	}
}

// TestDeployGenesisFileLoads guards the published canonical genesis: operators
// copy deploy/genesis.json to every public-devnet node, so it must always be
// valid and its declared fingerprint current.
func TestDeployGenesisFileLoads(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "genesis.json")
	spec, err := Load(path)
	if err != nil {
		t.Fatalf("published deploy/genesis.json is invalid: %v", err)
	}
	if spec.ChainID != "san-devnet-1" {
		t.Fatalf("deploy/genesis.json chain id: %q", spec.ChainID)
	}
	if len(spec.Fingerprint) != 64 {
		t.Fatalf("deploy/genesis.json fingerprint: %q", spec.Fingerprint)
	}
}

// TestEnvironment pins the SAN_* bridge used by the launcher.
func TestEnvironment(t *testing.T) {
	spec := validSpec(t)
	env, err := spec.Environment()
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	expectations := map[string]string{
		"SAN_CHAIN_ID":              spec.ChainID,
		"SAN_BLOCK_REWARD":          "2",
		"SAN_MIN_BLOCK_INTERVAL_MS": "1000",
		"SAN_PROPOSER_TIMEOUT":      "6",
		"SAN_BLOCK_GAS_LIMIT":       "30000000",
		"SAN_UNBONDING_PERIOD":      "100",
		"SAN_SLASH_BPS":             "5000",
		"SAN_MIN_VALIDATOR_STAKE":   "0",
		"SAN_GENESIS_FINGERPRINT":   spec.Fingerprint,
	}
	for name, expected := range expectations {
		if env[name] != expected {
			t.Errorf("%s: got %q, want %q", name, env[name], expected)
		}
	}
	if !strings.Contains(env["SAN_GENESIS_ALLOCATION"], ":10000") {
		t.Errorf("SAN_GENESIS_ALLOCATION: %q", env["SAN_GENESIS_ALLOCATION"])
	}
}
