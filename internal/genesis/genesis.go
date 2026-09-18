// Package genesis defines the canonical SAN genesis configuration file: the
// chain id, genesis allocations, consensus parameters and (optional) validator
// bootstrap list that every node on a network must share. The file carries a
// fingerprint (SHA-256 over a canonical encoding of those fields) that nodes
// compare at startup, in the HELLO handshake and before syncing, so a node
// cannot accidentally join a chain started from a different genesis.
//
// File shape (format_version 1):
//
//	{
//	  "format_version": 1,
//	  "chain_id": "san-devnet-1",
//	  "allocations": { "0x...": "10000" },
//	  "parameters": {
//	    "block_reward": "2",
//	    "min_block_interval_ms": 1000,
//	    "proposer_timeout_ms": 6000,
//	    "block_gas_limit": 30000000,
//	    "unbonding_period": 100,
//	    "slash_bps": 5000,
//	    "min_validator_stake": "0"
//	  },
//	  "validators": [],
//	  "fingerprint": "<sha256 hex>"
//	}
//
// Allocations and the SAN-denominated parameters accept a decimal SAN string
// (or a JSON number); allocations may use an address or an ML-DSA public key.
package genesis

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/ledger"
)

// FormatVersion is the genesis file format this package understands.
const FormatVersion = 1

// Parameters are the genesis consensus parameters as written in the file.
// SAN-denominated values are decimal strings so JSON floats can never change
// the fingerprint.
type Parameters struct {
	BlockReward        string
	MinBlockIntervalMs int64
	ProposerTimeoutMs  int64
	BlockGasLimit      int64
	UnbondingPeriod    int64
	SlashBps           int64
	MinValidatorStake  string
}

// UnitsParameters is the same parameter set in base units and milliseconds,
// the form used for fingerprinting and node configuration.
type UnitsParameters struct {
	BlockRewardUnits       int64
	MinBlockIntervalMs     int64
	ProposerTimeoutMs      int64
	BlockGasLimit          int64
	UnbondingPeriod        int64
	SlashBps               int64
	MinValidatorStakeUnits int64
}

// Spec is a validated genesis configuration.
type Spec struct {
	FormatVersion int
	ChainID       string
	// Allocations maps normalized addresses to SAN decimal amounts.
	Allocations map[string]string
	Parameters  Parameters
	// Validators are the bootstrap validators (addresses or public keys).
	Validators []string
	// Fingerprint is the SHA-256 hex of the canonical genesis fields.
	Fingerprint string
}

// fileDoc is the on-disk JSON shape, used only for Marshal.
type fileDoc struct {
	FormatVersion int               `json:"format_version"`
	ChainID       string            `json:"chain_id"`
	Allocations   map[string]string `json:"allocations"`
	Parameters    fileParameters    `json:"parameters"`
	Validators    []string          `json:"validators"`
	Fingerprint   string            `json:"fingerprint"`
}

type fileParameters struct {
	BlockReward        string `json:"block_reward"`
	MinBlockIntervalMs int64  `json:"min_block_interval_ms"`
	ProposerTimeoutMs  int64  `json:"proposer_timeout_ms"`
	BlockGasLimit      int64  `json:"block_gas_limit"`
	UnbondingPeriod    int64  `json:"unbonding_period"`
	SlashBps           int64  `json:"slash_bps"`
	MinValidatorStake  string `json:"min_validator_stake"`
}

var requiredParameterNames = []string{
	"block_reward",
	"min_block_interval_ms",
	"proposer_timeout_ms",
	"block_gas_limit",
	"unbonding_period",
	"slash_bps",
	"min_validator_stake",
}

// Load reads, parses and validates a genesis file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the genesis file %s: %w", path, err)
	}
	spec, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("genesis file %s: %w", path, err)
	}
	return spec, nil
}

// Parse validates a genesis document and resolves its fingerprint.
func Parse(data []byte) (*Spec, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var raw struct {
		FormatVersion json.Number    `json:"format_version"`
		ChainID       string         `json:"chain_id"`
		Allocations   map[string]any `json:"allocations"`
		Parameters    map[string]any `json:"parameters"`
		Validators    []string       `json:"validators"`
		Fingerprint   string         `json:"fingerprint"`
	}
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: trailing data after the genesis object")
	}

	if raw.FormatVersion == "" {
		return nil, fmt.Errorf("format_version is required")
	}
	version, err := strconv.Atoi(raw.FormatVersion.String())
	if err != nil {
		return nil, fmt.Errorf("format_version %q is not an integer", raw.FormatVersion.String())
	}
	if version != FormatVersion {
		return nil, fmt.Errorf(
			"unsupported format_version %d (this build understands %d)", version, FormatVersion)
	}

	spec := &Spec{
		FormatVersion: version,
		ChainID:       strings.TrimSpace(raw.ChainID),
		Allocations:   map[string]string{},
		Validators:    []string{},
	}
	if spec.ChainID == "" {
		return nil, fmt.Errorf("chain_id is required")
	}
	if strings.ContainsAny(spec.ChainID, " \t\r\n,:") {
		return nil, fmt.Errorf("chain_id %q must not contain whitespace, commas or colons", raw.ChainID)
	}

	allocations := map[string]int64{}
	for key, value := range raw.Allocations {
		normalized, err := normalizeKey(key)
		if err != nil {
			return nil, fmt.Errorf("allocation %q: %w", key, err)
		}
		if _, duplicate := allocations[normalized]; duplicate {
			return nil, fmt.Errorf("allocation %q appears more than once after normalization", key)
		}
		units, err := amountUnits(value)
		if err != nil {
			return nil, fmt.Errorf("allocation %q: %w", key, err)
		}
		if units < 0 {
			return nil, fmt.Errorf("allocation %q must not be negative", key)
		}
		allocations[normalized] = units
	}
	for address, units := range allocations {
		spec.Allocations[address] = ledger.UnitsToSAN(units)
	}

	if len(raw.Parameters) != len(requiredParameterNames) {
		missing := []string{}
		for _, name := range requiredParameterNames {
			if _, present := raw.Parameters[name]; !present {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("parameters are missing: %s", strings.Join(missing, ", "))
		}
	}
	for name := range raw.Parameters {
		if !isParameterName(name) {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	parameters, err := parseParameters(raw.Parameters)
	if err != nil {
		return nil, err
	}
	spec.Parameters = parameters

	for _, validator := range raw.Validators {
		normalized, err := normalizeKey(validator)
		if err != nil {
			return nil, fmt.Errorf("validator %q: %w", validator, err)
		}
		spec.Validators = append(spec.Validators, normalized)
	}
	spec.Validators = canonicalValidators(spec.Validators)

	fingerprint, err := spec.ComputeFingerprint()
	if err != nil {
		return nil, err
	}
	if provided := strings.TrimSpace(raw.Fingerprint); provided != "" {
		if !fingerprintMatches(provided, fingerprint) {
			return nil, fmt.Errorf(
				"fingerprint mismatch: the file declares %s but its contents hash to %s; "+
					"the file was edited without regenerating the fingerprint",
				provided, fingerprint)
		}
	}
	spec.Fingerprint = fingerprint
	return spec, nil
}

func isParameterName(name string) bool {
	for _, candidate := range requiredParameterNames {
		if candidate == name {
			return true
		}
	}
	return false
}

func parseParameters(raw map[string]any) (Parameters, error) {
	parameters := Parameters{}
	for _, name := range requiredParameterNames {
		value, present := raw[name]
		if !present {
			return parameters, fmt.Errorf("parameter %q is required", name)
		}
		switch name {
		case "block_reward":
			units, err := amountUnits(value)
			if err != nil {
				return parameters, fmt.Errorf("parameter block_reward: %w", err)
			}
			if !ledger.GovernanceValueOK(name, units) {
				return parameters, fmt.Errorf("parameter block_reward=%d is out of range", units)
			}
			parameters.BlockReward = ledger.UnitsToSAN(units)
		case "min_validator_stake":
			units, err := amountUnits(value)
			if err != nil {
				return parameters, fmt.Errorf("parameter min_validator_stake: %w", err)
			}
			if !ledger.GovernanceValueOK(name, units) {
				return parameters, fmt.Errorf("parameter min_validator_stake=%d is out of range", units)
			}
			parameters.MinValidatorStake = ledger.UnitsToSAN(units)
		default:
			parsed, err := intValue(value)
			if err != nil {
				return parameters, fmt.Errorf("parameter %s: %w", name, err)
			}
			if !ledger.GovernanceValueOK(name, parsed) {
				return parameters, fmt.Errorf("parameter %s=%d is out of range", name, parsed)
			}
			switch name {
			case "min_block_interval_ms":
				parameters.MinBlockIntervalMs = parsed
			case "proposer_timeout_ms":
				parameters.ProposerTimeoutMs = parsed
			case "block_gas_limit":
				parameters.BlockGasLimit = parsed
			case "unbonding_period":
				parameters.UnbondingPeriod = parsed
			case "slash_bps":
				parameters.SlashBps = parsed
			}
		}
	}
	return parameters, nil
}

// ComputeFingerprint hashes the canonical chain id, allocations, parameters
// and validator list. Two nodes with the same fingerprint can validate each
// other's blocks; any change to a genesis field changes it.
func (spec *Spec) ComputeFingerprint() (string, error) {
	if spec == nil {
		return "", fmt.Errorf("genesis is nil")
	}
	allocations := map[string]int64{}
	for address, amount := range spec.Allocations {
		units, err := ledger.SanToUnits(amount)
		if err != nil {
			return "", fmt.Errorf("allocation %q: %w", address, err)
		}
		allocations[address] = units
	}
	blockReward, err := ledger.SanToUnits(spec.Parameters.BlockReward)
	if err != nil {
		return "", fmt.Errorf("parameter block_reward: %w", err)
	}
	minStake, err := ledger.SanToUnits(spec.Parameters.MinValidatorStake)
	if err != nil {
		return "", fmt.Errorf("parameter min_validator_stake: %w", err)
	}
	return FingerprintFromUnits(spec.ChainID, allocations, UnitsParameters{
		BlockRewardUnits:       blockReward,
		MinBlockIntervalMs:     spec.Parameters.MinBlockIntervalMs,
		ProposerTimeoutMs:      spec.Parameters.ProposerTimeoutMs,
		BlockGasLimit:          spec.Parameters.BlockGasLimit,
		UnbondingPeriod:        spec.Parameters.UnbondingPeriod,
		SlashBps:               spec.Parameters.SlashBps,
		MinValidatorStakeUnits: minStake,
	}, spec.Validators)
}

// FingerprintFromUnits computes the genesis fingerprint from resolved values
// (base units). Netnode uses it directly so a node without a genesis file
// still has a stable network fingerprint.
func FingerprintFromUnits(chainID string, allocations map[string]int64, parameters UnitsParameters, validators []string) (string, error) {
	chainID = strings.TrimSpace(chainID)
	if chainID == "" {
		return "", fmt.Errorf("genesis chain id is required")
	}
	if parameters.BlockRewardUnits < 0 {
		return "", fmt.Errorf("block reward must not be negative")
	}
	if parameters.MinValidatorStakeUnits < 0 {
		return "", fmt.Errorf("min validator stake must not be negative")
	}
	normalizedAllocations := map[string]int64{}
	for key, units := range allocations {
		normalized, err := normalizeKey(key)
		if err != nil {
			return "", fmt.Errorf("allocation %q: %w", key, err)
		}
		if units < 0 {
			return "", fmt.Errorf("allocation %q must not be negative", key)
		}
		if _, duplicate := normalizedAllocations[normalized]; duplicate {
			return "", fmt.Errorf("allocation %q appears more than once after normalization", key)
		}
		normalizedAllocations[normalized] = units
	}
	payload := map[string]any{
		"format_version": int64(FormatVersion),
		"chain_id":       chainID,
		"allocations":    normalizedAllocations,
		"parameters": map[string]int64{
			"block_reward":          parameters.BlockRewardUnits,
			"min_block_interval_ms": parameters.MinBlockIntervalMs,
			"proposer_timeout_ms":   parameters.ProposerTimeoutMs,
			"block_gas_limit":       parameters.BlockGasLimit,
			"unbonding_period":      parameters.UnbondingPeriod,
			"slash_bps":             parameters.SlashBps,
			"min_validator_stake":   parameters.MinValidatorStakeUnits,
		},
		"validators": canonicalValidators(validators),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Marshal renders the file including its (recomputed) fingerprint.
func (spec *Spec) Marshal() ([]byte, error) {
	fingerprint, err := spec.ComputeFingerprint()
	if err != nil {
		return nil, err
	}
	allocations := map[string]string{}
	for address, amount := range spec.Allocations {
		units, err := ledger.SanToUnits(amount)
		if err != nil {
			return nil, fmt.Errorf("allocation %q: %w", address, err)
		}
		allocations[address] = ledger.UnitsToSAN(units)
	}
	document := fileDoc{
		FormatVersion: FormatVersion,
		ChainID:       spec.ChainID,
		Allocations:   allocations,
		Parameters: fileParameters{
			BlockReward:        spec.Parameters.BlockReward,
			MinBlockIntervalMs: spec.Parameters.MinBlockIntervalMs,
			ProposerTimeoutMs:  spec.Parameters.ProposerTimeoutMs,
			BlockGasLimit:      spec.Parameters.BlockGasLimit,
			UnbondingPeriod:    spec.Parameters.UnbondingPeriod,
			SlashBps:           spec.Parameters.SlashBps,
			MinValidatorStake:  spec.Parameters.MinValidatorStake,
		},
		Validators:  canonicalValidators(spec.Validators),
		Fingerprint: fingerprint,
	}
	return json.MarshalIndent(document, "", "  ")
}

// UnitAllocations returns the allocations in base units.
func (spec *Spec) UnitAllocations() (map[string]int64, error) {
	allocations := make(map[string]int64, len(spec.Allocations))
	for address, amount := range spec.Allocations {
		units, err := ledger.SanToUnits(amount)
		if err != nil {
			return nil, fmt.Errorf("allocation %q: %w", address, err)
		}
		allocations[address] = units
	}
	return allocations, nil
}

// Environment renders the SAN_* variables that configure a node from this
// genesis. It is the bridge used by the launcher and the installer.
func (spec *Spec) Environment() (map[string]string, error) {
	allocations, err := spec.UnitAllocations()
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(allocations))
	for address := range allocations {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	parts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parts = append(parts, address+":"+ledger.UnitsToSAN(allocations[address]))
	}
	fingerprint, err := spec.ComputeFingerprint()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"SAN_CHAIN_ID":              spec.ChainID,
		"SAN_GENESIS_ALLOCATION":    strings.Join(parts, ","),
		"SAN_BLOCK_REWARD":          spec.Parameters.BlockReward,
		"SAN_MIN_BLOCK_INTERVAL_MS": strconv.FormatInt(spec.Parameters.MinBlockIntervalMs, 10),
		"SAN_PROPOSER_TIMEOUT":      strconv.FormatFloat(float64(spec.Parameters.ProposerTimeoutMs)/1000.0, 'f', -1, 64),
		"SAN_BLOCK_GAS_LIMIT":       strconv.FormatInt(spec.Parameters.BlockGasLimit, 10),
		"SAN_UNBONDING_PERIOD":      strconv.FormatInt(spec.Parameters.UnbondingPeriod, 10),
		"SAN_SLASH_BPS":             strconv.FormatInt(spec.Parameters.SlashBps, 10),
		"SAN_MIN_VALIDATOR_STAKE":   spec.Parameters.MinValidatorStake,
		"SAN_GENESIS_VALIDATORS":    strings.Join(spec.Validators, ","),
		"SAN_GENESIS_FINGERPRINT":   fingerprint,
	}, nil
}

// ---------------------------------------------------------------------- #
// helpers
// ---------------------------------------------------------------------- #

func fingerprintMatches(provided, computed string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		return strings.TrimPrefix(value, "sha256:")
	}
	return normalize(provided) == normalize(computed)
}

func canonicalValidators(validators []string) []string {
	normalized := map[string]struct{}{}
	for _, validator := range validators {
		key, err := normalizeKey(validator)
		if err != nil {
			continue
		}
		normalized[key] = struct{}{}
	}
	result := make([]string, 0, len(normalized))
	for key := range normalized {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// normalizeKey resolves an address or an ML-DSA public key to an address.
func normalizeKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("key is empty")
	}
	if ledger.IsValidAddress(raw) {
		return ledger.NormalizeAddress(raw)
	}
	address, err := ledger.AddressFromPublicKey(raw)
	if err != nil {
		return "", fmt.Errorf("not an address or public key: %w", err)
	}
	return address, nil
}

// amountUnits accepts SAN decimal strings, JSON numbers and floats.
func amountUnits(value any) (int64, error) {
	switch typed := value.(type) {
	case string:
		return ledger.SanToUnits(typed)
	case json.Number:
		return ledger.SanToUnits(typed.String())
	case float64:
		return ledger.SanToUnits(typed)
	case float32:
		return ledger.SanToUnits(float64(typed))
	case int:
		return ledger.SanToUnits(int64(typed))
	case int64:
		return ledger.SanToUnits(typed)
	default:
		return 0, fmt.Errorf("expected a SAN amount, got %T", value)
	}
}

// intValue accepts whole JSON numbers and decimal integer strings.
func intValue(value any) (int64, error) {
	switch typed := value.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not an integer", typed)
		}
		return parsed, nil
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, fmt.Errorf("%q is not an integer", typed.String())
		}
		return parsed, nil
	case float64:
		if typed != float64(int64(typed)) {
			return 0, fmt.Errorf("%v is not an integer", typed)
		}
		return int64(typed), nil
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", value)
	}
}
