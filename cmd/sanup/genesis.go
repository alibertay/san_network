package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/sdk"
)

// resolveGenesis decides whether this node is a founder or a joiner and, for
// joiners, derives the seed address and the genesis environment from the
// seed's REST endpoint. Explicit --bootstrap wins; otherwise a reachable node
// in the local peer registry is used, and when there is none this node starts
// as the founder.
func resolveGenesis(opts options, publicKey string) (bool, string, map[string]string, error) {
	explicit := strings.TrimSpace(opts.bootstrap)
	if opts.seed.value == "true" {
		if explicit != "" {
			return false, "", nil, fmt.Errorf("--seed and --bootstrap are mutually exclusive")
		}
		return true, "", nil, nil
	}
	if opts.seed.value == "false" || explicit != "" {
		bootstrap, err := chooseBootstrap(explicit, opts)
		if err != nil {
			return false, "", nil, err
		}
		genesisEnv, err := fetchGenesisEnv(bootstrap)
		if err != nil {
			return false, "", nil, err
		}
		return false, bootstrap, genesisEnv, nil
	}
	bootstrap := findRegistryBootstrap(publicKey, opts)
	if bootstrap == "" {
		return true, "", nil, nil
	}
	genesisEnv, err := fetchGenesisEnv(bootstrap)
	if err != nil {
		return false, "", nil, err
	}
	return false, bootstrap, genesisEnv, nil
}

func chooseBootstrap(explicit string, opts options) (string, error) {
	if explicit == "" {
		bootstrap := findRegistryBootstrap("", opts)
		if bootstrap == "" {
			return "", fmt.Errorf("--seed false needs --bootstrap or a reachable node in the local peer registry")
		}
		return bootstrap, nil
	}
	candidates := []string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(explicit, ",") {
		candidate := normalizeBootstrap(raw)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("--bootstrap has no usable address")
	}
	unreachable := []string{}
	for _, candidate := range candidates {
		host, port, found := strings.Cut(candidate, ":")
		if !found {
			unreachable = append(unreachable, candidate)
			continue
		}
		if nodeHealthy(connectHost(host), atoiOr(port, 0), 1500*time.Millisecond) {
			return candidate, nil
		}
		unreachable = append(unreachable, candidate)
	}
	return "", fmt.Errorf("no --bootstrap endpoint is reachable (tried %s)", strings.Join(unreachable, ", "))
}

func normalizeBootstrap(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimSuffix(value, "/")
	if slash := strings.Index(value, "/"); slash >= 0 {
		value = value[:slash]
	}
	return value
}

// findRegistryBootstrap returns the first fresh, reachable, non-self registry
// entry as host:api_port.
func findRegistryBootstrap(publicKey string, opts options) string {
	path := netnode.DefaultPeerRegistryPath()
	if path == "" {
		return ""
	}
	ttl := netnode.DefaultDiscoveryTTL
	if raw := strings.TrimSpace(os.Getenv("SAN_DISCOVERY_TTL")); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	for _, record := range netnode.LoadPeerRegistry(path, ttl) {
		recordKey, _ := record["public_key"].(string)
		if publicKey != "" && recordKey == publicKey {
			continue
		}
		if chainID, _ := record["chain_id"].(string); chainID != "" && chainID != opts.chainID {
			continue
		}
		host := connectHost(anyToString(record["host"]))
		port := int(anyToInt64(record["api_port"]))
		if host == "" || port <= 0 || !nodeHealthy(host, port, 1500*time.Millisecond) {
			continue
		}
		return fmt.Sprintf("%s:%d", host, port)
	}
	return ""
}

func fetchGenesisEnv(bootstrap string) (map[string]string, error) {
	base := bootstrap
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	client := sdk.NewSanClient(base, nil, 5*time.Second)
	payload, err := client.Genesis()
	if err != nil {
		return nil, fmt.Errorf("cannot fetch the seed's genesis from %s: %w", base, err)
	}
	env := genesisEnvFromPayload(payload)
	if strings.TrimSpace(env["SAN_GENESIS_ALLOCATION"]) == "" {
		return nil, fmt.Errorf("the seed %s reported an empty genesis allocation", base)
	}
	return env, nil
}

// genesisEnvFromPayload mirrors scripts/run_node.py apply_genesis_parameters:
// the joiner must adopt the seed's genesis parameters or block verification
// diverges.
func genesisEnvFromPayload(payload map[string]any) map[string]string {
	env := map[string]string{}
	parameters, _ := payload["parameters"].(map[string]any)
	unitsToEnv := map[string]string{
		"block_reward":        "SAN_BLOCK_REWARD",
		"min_validator_stake": "SAN_MIN_VALIDATOR_STAKE",
	}
	for key, name := range unitsToEnv {
		if raw, ok := parameters[key]; ok {
			env[name] = ledger.UnitsToSAN(anyToInt64(raw))
		}
	}
	intToEnv := map[string]string{
		"unbonding_period":      "SAN_UNBONDING_PERIOD",
		"slash_bps":             "SAN_SLASH_BPS",
		"block_gas_limit":       "SAN_BLOCK_GAS_LIMIT",
		"min_block_interval_ms": "SAN_MIN_BLOCK_INTERVAL_MS",
	}
	for key, name := range intToEnv {
		if raw, ok := parameters[key]; ok {
			env[name] = strconv.FormatInt(anyToInt64(raw), 10)
		}
	}
	if raw, ok := parameters["proposer_timeout_ms"]; ok {
		env["SAN_PROPOSER_TIMEOUT"] = strconv.FormatFloat(float64(anyToInt64(raw))/1000.0, 'f', -1, 64)
	}
	if chainID, ok := payload["chain_id"]; ok && chainID != nil {
		env["SAN_CHAIN_ID"] = fmt.Sprintf("%v", chainID)
	}
	allocations, _ := payload["genesis_allocation"].(map[string]any)
	addresses := make([]string, 0, len(allocations))
	for address := range allocations {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	parts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parts = append(parts, address+":"+ledger.UnitsToSAN(anyToInt64(allocations[address])))
	}
	env["SAN_GENESIS_ALLOCATION"] = strings.Join(parts, ",")
	return env
}

// buildChildEnv prepares the SAN_* environment for the detached node process.
func buildChildEnv(opts options, dataDir, keyFile, publicKey string, rewardAddress string,
	ports nodePorts, seed bool, bootstrap string, genesisEnv map[string]string) []string {
	overrides := map[string]string{
		"SANUP_CHILD":             "1",
		"SAN_DB_BACKEND":          "memory",
		"SAN_DB_PATH":             filepath.Join(dataDir, "node.db"),
		"SAN_HOST":                opts.host,
		"SAN_ADVERTISE_HOST":      opts.host,
		"SAN_API_PORT":            strconv.Itoa(ports.API),
		"SAN_P2P_PORT":            strconv.Itoa(ports.P2P),
		"SAN_PEER_PORT":           strconv.Itoa(ports.Peer),
		"SAN_CONTROLLER_PORT":     strconv.Itoa(ports.Controller),
		"SAN_KEY_FILE":            keyFile,
		"SAN_REWARD_ADDRESS":      rewardAddress,
		"SAN_BLOCK_THRESHOLD_FEE": "0.0001",
		"SAN_CONTROLLER_COUNT":    "0",
		"SAN_DISCOVERY":           "1",
		"SAN_DISCOVERY_INTERVAL":  "2",
		"SAN_CHAIN_ID":            opts.chainID,
	}
	if seed {
		overrides["SAN_GENESIS_ALLOCATION"] = publicKey + ":" + opts.genesisAmount
		overrides["SAN_BLOCK_REWARD"] = "2"
		overrides["SAN_MIN_BLOCK_INTERVAL_MS"] = "1000"
		overrides["SAN_UNBONDING_PERIOD"] = "0"
		overrides["SAN_MIN_VALIDATOR_STAKE"] = "0"
	} else {
		for name, value := range genesisEnv {
			overrides[name] = value
		}
		overrides["SAN_BOOTSTRAP"] = bootstrap
	}
	return envList(overrides)
}

func anyToString(value any) string {
	return fmt.Sprintf("%v", value)
}

// anyToInt64 accepts the canonical JSON scalar shapes (int64, float64, string,
// json.Number) used by the REST payloads.
func anyToInt64(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}
