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
)

// resolveGenesis decides whether this node is a founder or a joiner and, for
// joiners, derives the seed address and the genesis environment from the
// seed's REST endpoint. Explicit --bootstrap wins; otherwise a reachable node
// in the local peer registry is used, and when there is none this node starts
// as the founder. expectedFingerprint, when non-empty, pins the seed to the
// same full genesis fingerprint (from a canonical genesis file).
func resolveGenesis(opts options, publicKey, expectedFingerprint string) (bool, string, map[string]string, error) {
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
		genesisEnv, err := fetchGenesisEnv(bootstrap, opts.apiToken, expectedFingerprint)
		if err != nil {
			return false, "", nil, err
		}
		return false, bootstrap, genesisEnv, nil
	}
	// Default (auto): wide-area seeds first, then the persisted peer cache,
	// then the local registry, and otherwise start as the founder. A previous
	// join is reused from sanup.json when no seed is reachable.
	bootstrap := ""
	for _, candidate := range seedRESTAddresses(opts.seeds, 8000) {
		host, port, found := strings.Cut(candidate, ":")
		if found && nodeHealthy(connectHost(host), atoiOr(port, 0), 1500*time.Millisecond) {
			bootstrap = candidate
			break
		}
	}
	if bootstrap == "" {
		bootstrap = findCacheBootstrap(publicKey, opts)
	}
	if bootstrap == "" && !opts.noRegistry {
		bootstrap = findRegistryBootstrap(publicKey, opts)
	}
	if bootstrap == "" {
		if state := readState(opts.dataDir); len(state.GenesisEnv) > 0 {
			return false, "", state.GenesisEnv, nil
		}
		return true, "", nil, nil
	}
	genesisEnv, err := fetchGenesisEnv(bootstrap, opts.apiToken, expectedFingerprint)
	if err != nil {
		return false, "", nil, err
	}
	return false, bootstrap, genesisEnv, nil
}

// seedRESTAddresses maps --seeds entries to REST endpoints for the genesis
// fetch: explicit host:port entries are kept, bare hosts get the default API
// port (8000).
func seedRESTAddresses(seeds string, apiPort int) []string {
	addresses := []string{}
	for _, raw := range strings.Split(seeds, ",") {
		candidate := normalizeBootstrap(raw)
		if candidate == "" {
			continue
		}
		if !strings.Contains(candidate, ":") {
			candidate = candidate + ":" + strconv.Itoa(apiPort)
		}
		addresses = append(addresses, candidate)
	}
	return addresses
}

// seedPeerAddresses maps --seeds entries to P2P endpoints: bare hosts get the
// default peer session port (8770).
func seedPeerAddresses(seeds string, peerPort int) []string {
	addresses := []string{}
	for _, raw := range strings.Split(seeds, ",") {
		candidate := normalizeBootstrap(raw)
		if candidate == "" {
			continue
		}
		if !strings.Contains(candidate, ":") {
			candidate = candidate + ":" + strconv.Itoa(peerPort)
		}
		addresses = append(addresses, candidate)
	}
	return addresses
}

func chooseBootstrap(explicit string, opts options) (string, error) {
	if explicit == "" {
		for _, candidate := range seedRESTAddresses(opts.seeds, 8000) {
			host, port, found := strings.Cut(candidate, ":")
			if !found {
				continue
			}
			if nodeHealthy(connectHost(host), atoiOr(port, 0), 1500*time.Millisecond) {
				return candidate, nil
			}
		}
		if bootstrap := findCacheBootstrap("", opts); bootstrap != "" {
			return bootstrap, nil
		}
		if !opts.noRegistry {
			if bootstrap := findRegistryBootstrap("", opts); bootstrap != "" {
				return bootstrap, nil
			}
		}
		return "", fmt.Errorf("--seed false needs --bootstrap, a reachable --seeds entry (directly or through the peer cache) or a reachable node in the local peer registry")
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

// findCacheBootstrap returns the first reachable peer from the persisted
// address cache (as host:api_port) so a restarted joiner can re-fetch the
// genesis environment from the seed it learned before, without --seeds.
func findCacheBootstrap(publicKey string, opts options) string {
	path := strings.TrimSpace(opts.peerCache)
	if path == "" {
		dataDir, err := filepath.Abs(opts.dataDir)
		if err != nil {
			return ""
		}
		path = filepath.Join(dataDir, netnode.DefaultPeerCacheFile)
	}
	for _, record := range netnode.LoadPeerCache(path) {
		recordKey, _ := record["public_key"].(string)
		if publicKey != "" && recordKey == publicKey {
			continue
		}
		if chainID, _ := record["chain_id"].(string); chainID != "" && chainID != opts.chainID {
			continue
		}
		host := connectHost(anyToString(record["host"]))
		port := int(anyToInt64(record["api_port"]))
		if host == "" || port <= 0 {
			continue
		}
		if !nodeHealthy(host, port, 1500*time.Millisecond) {
			continue
		}
		return fmt.Sprintf("%s:%d", host, port)
	}
	return ""
}

func fetchGenesisEnv(bootstrap, token, expectedFingerprint string) (map[string]string, error) {
	base := bootstrap
	if !strings.Contains(base, "://") {
		scheme := "http"
		if localAPITLS {
			scheme = "https"
		}
		base = scheme + "://" + base
	}
	client := newClient(base, nil, 5*time.Second, token)
	payload, err := client.Genesis()
	if err != nil {
		return nil, fmt.Errorf("cannot fetch the seed's genesis from %s: %w", base, err)
	}
	env := genesisEnvFromPayload(payload)
	expected := strings.TrimSpace(expectedFingerprint)
	if expected == "" && strings.TrimSpace(env["SAN_GENESIS_ALLOCATION"]) == "" {
		return nil, fmt.Errorf("the seed %s reported an empty genesis allocation", base)
	}
	if expected != "" {
		reported := strings.TrimSpace(env["SAN_GENESIS_FINGERPRINT"])
		if reported == "" {
			return nil, fmt.Errorf(
				"the seed %s does not report a full genesis fingerprint (pre-Batch-E node); "+
					"refusing to join without a verifiable genesis", base)
		}
		if !strings.EqualFold(normalizeFingerprint(reported), normalizeFingerprint(expected)) {
			return nil, fmt.Errorf(
				"genesis mismatch: the seed %s is on %s while this node expects %s; "+
					"refusing to join a different network", base, reported, expected)
		}
	}
	return env, nil
}

func normalizeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.TrimPrefix(value, "sha256:")
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
	if fingerprint, ok := payload["genesis_fingerprint"]; ok && fingerprint != nil {
		if text := strings.TrimSpace(fmt.Sprintf("%v", fingerprint)); text != "" {
			env["SAN_GENESIS_FINGERPRINT"] = text
		}
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
	advertise := strings.TrimSpace(opts.advertiseHost)
	if advertise == "" && opts.host != "0.0.0.0" && opts.host != "::" {
		advertise = opts.host
	}
	overrides := map[string]string{
		"SANUP_CHILD":             "1",
		"SAN_HOST":                opts.host,
		"SAN_API_HOST":            opts.apiHost,
		"SAN_API_PORT":            strconv.Itoa(ports.API),
		"SAN_P2P_PORT":            strconv.Itoa(ports.P2P),
		"SAN_PEER_PORT":           strconv.Itoa(ports.Peer),
		"SAN_CONTROLLER_PORT":     strconv.Itoa(ports.Controller),
		"SAN_KEY_FILE":            keyFile,
		"SAN_REWARD_ADDRESS":      rewardAddress,
		"SAN_BLOCK_THRESHOLD_FEE": "0.0001",
		"SAN_DISCOVERY_INTERVAL":  "2",
		"SAN_CHAIN_ID":            opts.chainID,
	}
	// Dev/bootstrap nodes select no controllers; public-devnet mode keeps the
	// operator's SAN_CONTROLLER_COUNT (or the config default) so the preflight
	// can enforce the minimum controller set.
	public := opts.publicDevnet || sanupEnvTruthy("SAN_PUBLIC_DEVNET")
	if !public {
		overrides["SAN_CONTROLLER_COUNT"] = "0"
	}
	if public {
		overrides["SAN_PUBLIC_DEVNET"] = "1"
	}
	if opts.allowInsecure {
		overrides["SAN_ALLOW_INSECURE_PUBLIC"] = "1"
	}
	if strings.TrimSpace(opts.genesisFile) != "" {
		overrides["SAN_GENESIS_FILE"] = strings.TrimSpace(opts.genesisFile)
	}
	// A systemd EnvironmentFile (or a shell export) may select LMDB and a
	// persistent database path; only fall back to the devnet memory defaults
	// when the operator did not choose one. Public-devnet mode never selects
	// the ephemeral backend: the node preflight rejects it instead.
	if _, set := os.LookupEnv("SAN_DB_BACKEND"); !set && !public {
		overrides["SAN_DB_BACKEND"] = "memory"
	}
	if _, set := os.LookupEnv("SAN_DB_PATH"); !set {
		overrides["SAN_DB_PATH"] = filepath.Join(dataDir, "node.db")
	}
	if advertise != "" {
		overrides["SAN_ADVERTISE_HOST"] = advertise
	}
	if opts.noRegistry {
		overrides["SAN_DISCOVERY"] = "0"
		overrides["SAN_PEER_REGISTRY"] = ""
	} else if _, set := os.LookupEnv("SAN_DISCOVERY"); !set {
		overrides["SAN_DISCOVERY"] = "1"
	}
	if opts.registryPath != "" {
		overrides["SAN_PEER_REGISTRY"] = opts.registryPath
	}
	if opts.peerCache != "" {
		overrides["SAN_PEERS_CACHE"] = opts.peerCache
	}
	if opts.tlsCert != "" {
		overrides["SAN_TLS_CERT"] = opts.tlsCert
		overrides["SAN_TLS_KEY"] = opts.tlsKey
	}
	if opts.tlsCA != "" {
		overrides["SAN_TLS_CA"] = opts.tlsCA
	}
	if opts.apiToken != "" {
		overrides["SAN_API_TOKEN"] = opts.apiToken
	}
	if opts.faucet {
		overrides["SAN_FAUCET"] = "1"
	}
	if value := strings.TrimSpace(opts.faucetAmount); value != "" {
		overrides["SAN_FAUCET_AMOUNT"] = value
	}
	if value := strings.TrimSpace(opts.faucetMax); value != "" {
		overrides["SAN_FAUCET_MAX"] = value
	}
	if value := strings.TrimSpace(opts.faucetCooldown); value != "" {
		overrides["SAN_FAUCET_COOLDOWN"] = value
	}
	if seeds := strings.TrimSpace(opts.seeds); seeds != "" {
		overrides["SAN_DNS_SEEDS"] = seeds
	}
	bootstrapAddresses := []string{}
	if strings.TrimSpace(bootstrap) != "" {
		bootstrapAddresses = append(bootstrapAddresses, strings.TrimSpace(bootstrap))
	}
	bootstrapAddresses = append(bootstrapAddresses, seedPeerAddresses(opts.seeds, ports.Peer)...)
	if len(bootstrapAddresses) > 0 {
		overrides["SAN_BOOTSTRAP"] = strings.Join(bootstrapAddresses, ",")
	}
	if seed {
		if strings.TrimSpace(opts.genesisFile) != "" {
			// The canonical genesis file is authoritative; no dynamic premine.
			for name, value := range genesisEnv {
				overrides[name] = value
			}
		} else {
			overrides["SAN_GENESIS_ALLOCATION"] = publicKey + ":" + opts.genesisAmount
			overrides["SAN_BLOCK_REWARD"] = "2"
			overrides["SAN_MIN_BLOCK_INTERVAL_MS"] = "1000"
			overrides["SAN_UNBONDING_PERIOD"] = "0"
			overrides["SAN_MIN_VALIDATOR_STAKE"] = "0"
		}
	} else {
		for name, value := range genesisEnv {
			overrides[name] = value
		}
	}
	return envList(overrides)
}

func anyToString(value any) string {
	return fmt.Sprintf("%v", value)
}

// sanupEnvTruthy reports whether a SAN_* environment flag is enabled.
func sanupEnvTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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
