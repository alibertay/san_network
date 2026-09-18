package netnode

// Genesis binding for the Go node: the full genesis fingerprint, the genesis
// file bridge and the HELLO handshake compatibility helpers. Kept in a
// separate file so the consensus implementation stays focused.

import (
	"fmt"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/genesis"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/version"
)

// HELLO capabilities. A protocol-version-3 peer must advertise every required
// capability; unknown capabilities are ignored (additive evolution).
const (
	// CapabilityGenesisFingerprint is the full genesis fingerprint binding.
	CapabilityGenesisFingerprint = "genesis-fingerprint-v1"
	// CapabilitySoftwareVersion is the semantic software version field.
	CapabilitySoftwareVersion = "software-version-v1"
)

// requiredHelloCapabilities is the minimum capability set for a v3 peer.
var requiredHelloCapabilities = []string{CapabilityGenesisFingerprint, CapabilitySoftwareVersion}

// supportedHelloCapabilities is advertised by this node, sorted.
func supportedHelloCapabilities() []string {
	return []string{CapabilityGenesisFingerprint, CapabilitySoftwareVersion}
}

// hasRequiredCapabilities reports whether the HELLO capability list contains
// every capability this protocol version requires.
func hasRequiredCapabilities(raw any) bool {
	advertised := map[string]struct{}{}
	switch typed := raw.(type) {
	case []any:
		for _, item := range typed {
			if capability, ok := item.(string); ok {
				advertised[capability] = struct{}{}
			}
		}
	case []string:
		for _, capability := range typed {
			advertised[capability] = struct{}{}
		}
	default:
		return false
	}
	for _, capability := range requiredHelloCapabilities {
		if _, present := advertised[capability]; !present {
			return false
		}
	}
	return true
}

// GenesisFingerprintFromConfig computes the full genesis fingerprint of a
// resolved node configuration (chain id, normalized allocations, consensus
// parameters and the validator bootstrap list).
func GenesisFingerprintFromConfig(config NodeConfig) (string, error) {
	blockReward, err := ledger.SanToUnits(config.BlockReward)
	if err != nil {
		return "", fmt.Errorf("invalid block reward: %w", err)
	}
	return genesis.FingerprintFromUnits(config.ChainID, config.GenesisAllocations, genesis.UnitsParameters{
		BlockRewardUnits:       blockReward,
		MinBlockIntervalMs:     config.MinBlockIntervalMs,
		ProposerTimeoutMs:      int64(config.ProposerTimeout * 1000),
		BlockGasLimit:          config.BlockGasLimit,
		UnbondingPeriod:        config.UnbondingPeriod,
		SlashBps:               config.SlashBps,
		MinValidatorStakeUnits: config.MinValidatorStake,
	}, config.GenesisValidators)
}

// GenesisFingerprint returns the node's full genesis fingerprint.
func (n *Node) GenesisFingerprint() string { return n.genesisFingerprint }

// SoftwareVersion returns the semantic version reported in the handshake.
func (n *Node) SoftwareVersion() string {
	return version.Resolve(ProtocolVersion, ledger.SchemaVersion).Version
}

// HelloCapabilities returns the capabilities advertised in HELLO.
func (n *Node) HelloCapabilities() []string {
	return supportedHelloCapabilities()
}

// ApplyGenesisFile loads config.GenesisFile (SAN_GENESIS_FILE) and applies its
// values. The file is authoritative for the genesis fields; an explicitly set
// SAN_CHAIN_ID or SAN_GENESIS_FINGERPRINT that conflicts with the file is a
// startup error instead of a silent override. Called once before validation.
func (config NodeConfig) ApplyGenesisFile() (NodeConfig, error) {
	path := strings.TrimSpace(config.GenesisFile)
	if path == "" {
		return config, nil
	}
	spec, err := genesis.Load(path)
	if err != nil {
		return config, err
	}
	if explicit, ok := envString("SAN_CHAIN_ID"); ok && explicit != spec.ChainID {
		return config, fmt.Errorf(
			"genesis file %s defines chain id %q but SAN_CHAIN_ID=%q; refusing to mix two networks",
			path, spec.ChainID, explicit)
	}
	if explicit, ok := envString("SAN_GENESIS_FINGERPRINT"); ok {
		if explicit != spec.Fingerprint {
			return config, fmt.Errorf(
				"genesis file %s has fingerprint %s but SAN_GENESIS_FINGERPRINT=%s; refusing to start",
				path, spec.Fingerprint, explicit)
		}
	}
	allocations, err := spec.UnitAllocations()
	if err != nil {
		return config, err
	}
	blockReward, err := ledger.SanToUnits(spec.Parameters.BlockReward)
	if err != nil {
		return config, fmt.Errorf("genesis file %s: block_reward: %w", path, err)
	}
	minStake, err := ledger.SanToUnits(spec.Parameters.MinValidatorStake)
	if err != nil {
		return config, fmt.Errorf("genesis file %s: min_validator_stake: %w", path, err)
	}
	config.ChainID = spec.ChainID
	config.GenesisAllocations = allocations
	config.BlockReward = float64(blockReward) / float64(ledger.SANBase)
	config.MinValidatorStake = minStake
	config.UnbondingPeriod = spec.Parameters.UnbondingPeriod
	config.SlashBps = spec.Parameters.SlashBps
	config.BlockGasLimit = spec.Parameters.BlockGasLimit
	config.ProposerTimeout = float64(spec.Parameters.ProposerTimeoutMs) / 1000.0
	config.MinBlockIntervalMs = spec.Parameters.MinBlockIntervalMs
	config.GenesisValidators = append([]string{}, spec.Validators...)
	config.GenesisFingerprint = spec.Fingerprint

	// Guard against float round-trip drift for exotic values: the config must
	// still hash to the file's fingerprint.
	recomputed, err := GenesisFingerprintFromConfig(config)
	if err != nil {
		return config, err
	}
	if recomputed != spec.Fingerprint {
		return config, fmt.Errorf(
			"genesis file %s does not round-trip through the node configuration "+
				"(file %s, config %s); check the parameter precision",
			path, spec.Fingerprint, recomputed)
	}
	return config, nil
}

// outgoingProtocolVersion is the protocol announced in HELLO: the current
// version unless legacy compatibility mode was requested explicitly.
func (n *Node) outgoingProtocolVersion() int64 {
	if n.config.AllowLegacyHandshake {
		return LegacyProtocolVersion
	}
	return int64(ProtocolVersion)
}

// helloRejectReason validates a HELLO/HELLO_ACK record and returns "" when it
// is acceptable. Every non-empty result is a machine-readable rejection reason
// that is counted in the metrics.
func (n *Node) helloRejectReason(data map[string]any) string {
	if data == nil {
		return "malformed"
	}
	messageType, _ := data["type"].(string)
	if messageType != "HELLO" && messageType != "HELLO_ACK" {
		return "type"
	}
	protocol, ok := int64Strict(data["protocol"])
	if !ok {
		return "malformed"
	}
	legacy := protocol == LegacyProtocolVersion
	if protocol != int64(ProtocolVersion) && !(legacy && n.config.AllowLegacyHandshake) {
		return "protocol"
	}
	if chainID, _ := data["chain_id"].(string); chainID != n.chainID {
		return "chain"
	}
	if !legacy {
		peerGenesis, _ := data["genesis"].(string)
		if peerGenesis == "" || !genesisFingerprintsEqual(peerGenesis, n.genesisFingerprint) {
			return "genesis"
		}
		software, _ := data["software"].(string)
		if strings.TrimSpace(software) == "" {
			return "software"
		}
		if !hasRequiredCapabilities(data["capabilities"]) {
			return "capability"
		}
	}
	timestamp, ok := numericValue(data["timestamp"])
	if !ok {
		return "malformed"
	}
	if absFloat(nowSeconds()-timestamp) > HelloTTL {
		return "stale"
	}
	publicKey, _ := data["public_key"].(string)
	signature, _ := data["signature"].(string)
	if publicKey == "" || signature == "" {
		return "malformed"
	}
	payload := mapWithout(data, helloMetaFields)
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return "signature"
	}
	if !ledger.VerifyIdentity(encoded, signature, publicKey) {
		return "signature"
	}
	return ""
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func genesisFingerprintsEqual(left, right string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		return strings.TrimPrefix(value, "sha256:")
	}
	return normalize(left) == normalize(right)
}
