// Package netnode is the Go port of the SAN Network node: configuration,
// gRPC transport, peer management, consensus and the node's RPC surface.
package netnode

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/ledger"
)

// Wide-area discovery defaults (Bitcoin-inspired).
const (
	// DefaultMaxAddrEntries caps the persisted address manager.
	DefaultMaxAddrEntries = 1024
	// DefaultOutboundPeers is the number of outbound slots the node maintains.
	DefaultOutboundPeers = 8
	// DefaultPeerCacheFile is the address-manager file name.
	DefaultPeerCacheFile = "peers-cache.json"
)

// DefaultChainID is the devnet chain id.
const DefaultChainID = "san-devnet-1"

// NodeConfig mirrors network/config.py NodeConfig.
type NodeConfig struct {
	Host               string
	APIPort            int
	ChainID            string
	P2PPort            int
	PeerPort           int
	ControllerPort     int
	Bootstrap          *string
	AdvertiseHost      *string
	ControllerCount    int
	BlockThresholdFee  float64
	BlockGasLimit      int64
	MinValidatorStake  int64
	UnbondingPeriod    int64
	SlashBps           int64
	PeerCheckInterval  float64
	WSTimeout          float64
	GenesisAllocations map[string]int64
	RequireBlockSig    bool
	BlockReward        float64
	MinBlockIntervalMs int64
	RewardAddress      string
	RequireStateRoot   bool
	KeyFile            *string
	MaxPeers           int
	WSMaxSize          int
	PeerRateLimit      int
	PeerRateWindow     float64
	PeerRecordTTL      float64
	PeerMissThreshold  int
	ControllerMinStake int64
	EpochLength        int
	ProposerTimeout    float64
	MaxProposerRounds  int
	MaxOrphans         int
	MaxReorgDepth      int
	BlockGossip        bool
	TLSCert            *string
	TLSKey             *string
	TLSCA              *string
	DBPath             *string
	DBBackend          string
	SyncBatchSize      int
	SyncMaxBlocks      int
	SnapshotInterval   int
	PruneKeep          int
	RPCRateLimit       int
	RPCRateWindow      float64
	RPCMaxBody         int
	MaxMempool         int

	// Local peer discovery through the file-backed peer registry.
	DiscoveryEnabled  bool
	PeerRegistryPath  string
	DiscoveryInterval float64
	DiscoveryTTL      float64
	DiscoveryProbe    bool

	// Wide-area discovery (Bitcoin/Ethereum style).
	DNSSeeds       []string // DNS seed hostnames (optionally host:port)
	BootstrapPeers []string // explicit seed addresses host:port
	PeerCachePath  string   // addrman persistence; default next to DBPath
	MaxAddrEntries int
	OutboundPeers  int

	// Public API hardening.
	APIHost  string // REST bind host; empty means Host
	APIToken string // optional bearer token

	// Faucet (enabled with SAN_FAUCET=1). Amounts are in base units.
	FaucetEnabled  bool
	FaucetAmount   int64
	FaucetMax      int64
	FaucetCooldown float64 // seconds per address and per IP
}

// DefaultNodeConfig returns the Python dataclass defaults.
func DefaultNodeConfig() NodeConfig {
	return NodeConfig{
		Host:               "0.0.0.0",
		APIPort:            8000,
		ChainID:            DefaultChainID,
		P2PPort:            8765,
		PeerPort:           8770,
		ControllerPort:     8769,
		ControllerCount:    10,
		BlockThresholdFee:  500.0,
		BlockGasLimit:      30_000_000,
		MinValidatorStake:  ledger.MinValidatorStakeUnits,
		UnbondingPeriod:    ledger.DefaultUnbondingPeriod,
		SlashBps:           ledger.DefaultSlashBps,
		PeerCheckInterval:  30.0,
		WSTimeout:          3.0,
		GenesisAllocations: map[string]int64{},
		RequireBlockSig:    true,
		RewardAddress:      "",
		RequireStateRoot:   true,
		MaxPeers:           64,
		WSMaxSize:          1 << 20,
		PeerRateLimit:      60,
		PeerRateWindow:     10.0,
		PeerRecordTTL:      300.0,
		PeerMissThreshold:  2,
		EpochLength:        100,
		ProposerTimeout:    6.0,
		MaxProposerRounds:  16,
		MaxOrphans:         64,
		MaxReorgDepth:      64,
		BlockGossip:        true,
		DBBackend:          "lmdb",
		SyncBatchSize:      128,
		SyncMaxBlocks:      50_000,
		SnapshotInterval:   1000,
		RPCRateLimit:       120,
		RPCRateWindow:      10.0,
		RPCMaxBody:         1 << 20,
		MaxMempool:         8192,
		DiscoveryInterval:  DefaultDiscoveryInterval,
		DiscoveryTTL:       DefaultDiscoveryTTL,
		DiscoveryProbe:     true,
		MaxAddrEntries:     DefaultMaxAddrEntries,
		OutboundPeers:      DefaultOutboundPeers,
		FaucetAmount:       10 * ledger.SANBase,
		FaucetMax:          100 * ledger.SANBase,
		FaucetCooldown:     60.0,
	}
}

// TLSEnabled reports whether both certificate and key are configured.
func (config NodeConfig) TLSEnabled() bool {
	return config.TLSCert != nil && config.TLSKey != nil
}

// NormalizeGenesisKeys resolves address/public-key keys to addresses.
func NormalizeGenesisKeys(allocations map[string]int64) map[string]int64 {
	normalized := map[string]int64{}
	for key, units := range allocations {
		resolved, err := NormalizeAllocationKey(key)
		if err != nil {
			log.Printf("Ignoring genesis allocation %q: %v", key, err)
			continue
		}
		normalized[resolved] = units
	}
	return normalized
}

// NormalizeAllocationKey accepts an address or a hex public key.
func NormalizeAllocationKey(raw string) (string, error) {
	if ledger.IsValidAddress(raw) {
		return ledger.NormalizeAddress(raw)
	}
	return ledger.AddressFromPublicKey(raw)
}

func envString(name string) (string, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

func envBool(name string, fallback bool) bool {
	raw, ok := envString(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envInt(name string, fallback int64) int64 {
	raw, ok := envString(name)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		log.Printf("Invalid %s=%q, using default %d", name, raw, fallback)
		return fallback
	}
	return value
}

func envFloat(name string, fallback float64) float64 {
	raw, ok := envString(name)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("Invalid %s=%q, using default %v", name, raw, fallback)
		return fallback
	}
	return value
}

func envSANUnits(name string, fallbackSAN float64) int64 {
	fallback, _ := ledger.SanToUnits(fallbackSAN)
	raw, ok := envString(name)
	if !ok {
		return fallback
	}
	value, err := ledger.SanToUnits(raw)
	if err != nil {
		log.Printf("Invalid %s=%q: %v", name, raw, err)
		return fallback
	}
	return value
}

// NodeConfigFromEnv reads SAN_* environment variables.
func NodeConfigFromEnv() NodeConfig {
	config := DefaultNodeConfig()
	config.Host = firstNonEmpty(envStringValue("SAN_HOST"), "0.0.0.0")
	config.ChainID = firstNonEmpty(envStringValue("SAN_CHAIN_ID"), DefaultChainID)
	config.APIPort = int(envInt("SAN_API_PORT", 8000))
	config.P2PPort = int(envInt("SAN_P2P_PORT", 8765))
	config.PeerPort = int(envInt("SAN_PEER_PORT", 8770))
	config.ControllerPort = int(envInt("SAN_CONTROLLER_PORT", 8769))
	config.Bootstrap = optionalEnv("SAN_BOOTSTRAP")
	config.AdvertiseHost = optionalEnv("SAN_ADVERTISE_HOST")
	config.ControllerCount = int(envInt("SAN_CONTROLLER_COUNT", 10))
	config.BlockThresholdFee = envFloat("SAN_BLOCK_THRESHOLD_FEE", 500.0)
	config.BlockGasLimit = envInt("SAN_BLOCK_GAS_LIMIT", 30_000_000)
	config.MinValidatorStake = envSANUnits("SAN_MIN_VALIDATOR_STAKE", 1000.0)
	config.UnbondingPeriod = envInt("SAN_UNBONDING_PERIOD", ledger.DefaultUnbondingPeriod)
	config.SlashBps = envInt("SAN_SLASH_BPS", ledger.DefaultSlashBps)
	config.PeerCheckInterval = envFloat("SAN_PEER_CHECK_INTERVAL", 30.0)
	config.WSTimeout = envFloat("SAN_WS_TIMEOUT", 3.0)
	config.GenesisAllocations = ParseGenesisAllocations(envStringValue("SAN_GENESIS_ALLOCATION"))
	config.RequireBlockSig = envBool("SAN_REQUIRE_BLOCK_SIGNATURE", true)
	config.BlockReward = envFloat("SAN_BLOCK_REWARD", 0.0)
	config.MinBlockIntervalMs = envInt("SAN_MIN_BLOCK_INTERVAL_MS", 0)
	config.RewardAddress = envStringValue("SAN_REWARD_ADDRESS")
	config.RequireStateRoot = envBool("SAN_REQUIRE_STATE_ROOT", true)
	config.KeyFile = optionalEnv("SAN_KEY_FILE")
	config.MaxPeers = int(envInt("SAN_MAX_PEERS", 64))
	config.WSMaxSize = int(envInt("SAN_WS_MAX_SIZE", 1<<20))
	config.PeerRateLimit = int(envInt("SAN_PEER_RATE_LIMIT", 60))
	config.PeerRateWindow = envFloat("SAN_PEER_RATE_WINDOW", 10.0)
	config.PeerRecordTTL = envFloat("SAN_PEER_TTL", 300.0)
	config.PeerMissThreshold = int(envInt("SAN_PEER_MISS_THRESHOLD", 2))
	config.ControllerMinStake = envSANUnits("SAN_CONTROLLER_MIN_STAKE", 0.0)
	config.EpochLength = int(envInt("SAN_EPOCH_LENGTH", 100))
	config.ProposerTimeout = envFloat("SAN_PROPOSER_TIMEOUT", 6.0)
	config.MaxProposerRounds = int(envInt("SAN_MAX_PROPOSER_ROUNDS", 16))
	config.MaxOrphans = int(envInt("SAN_MAX_ORPHANS", 64))
	config.MaxReorgDepth = int(envInt("SAN_MAX_REORG_DEPTH", 64))
	config.BlockGossip = envBool("SAN_BLOCK_GOSSIP", true)
	config.TLSCert = optionalEnv("SAN_TLS_CERT")
	config.TLSKey = optionalEnv("SAN_TLS_KEY")
	config.TLSCA = optionalEnv("SAN_TLS_CA")
	config.DBPath = optionalEnv("SAN_DB_PATH")
	config.DBBackend = firstNonEmpty(envStringValue("SAN_DB_BACKEND"), "lmdb")
	config.SyncBatchSize = int(envInt("SAN_SYNC_BATCH", 128))
	config.SyncMaxBlocks = int(envInt("SAN_SYNC_MAX_BLOCKS", 50_000))
	config.SnapshotInterval = int(envInt("SAN_SNAPSHOT_INTERVAL", 1000))
	config.PruneKeep = int(envInt("SAN_PRUNE_KEEP", 0))
	config.RPCRateLimit = int(envInt("SAN_RPC_RATE_LIMIT", 120))
	config.RPCRateWindow = envFloat("SAN_RPC_RATE_WINDOW", 10.0)
	config.RPCMaxBody = int(envInt("SAN_RPC_MAX_BODY", 1<<20))
	config.MaxMempool = int(envInt("SAN_MAX_MEMPOOL", 8192))
	config.DiscoveryEnabled = envBool("SAN_DISCOVERY", false)
	config.PeerRegistryPath = envStringValue("SAN_PEER_REGISTRY")
	config.DiscoveryInterval = envFloat("SAN_DISCOVERY_INTERVAL", DefaultDiscoveryInterval)
	config.DiscoveryTTL = envFloat("SAN_DISCOVERY_TTL", DefaultDiscoveryTTL)
	config.DiscoveryProbe = envBool("SAN_DISCOVERY_PROBE", true)
	config.DNSSeeds = splitList(envStringValue("SAN_DNS_SEEDS"))
	config.BootstrapPeers = splitList(envStringValue("SAN_BOOTSTRAP"))
	config.PeerCachePath = envStringValue("SAN_PEERS_CACHE")
	config.MaxAddrEntries = int(envInt("SAN_MAX_ADDR_ENTRIES", DefaultMaxAddrEntries))
	if config.MaxAddrEntries < 1 {
		config.MaxAddrEntries = DefaultMaxAddrEntries
	}
	config.OutboundPeers = int(envInt("SAN_OUTBOUND_PEERS", DefaultOutboundPeers))
	if config.OutboundPeers < 0 {
		config.OutboundPeers = DefaultOutboundPeers
	}
	config.APIHost = envStringValue("SAN_API_HOST")
	config.APIToken = envStringValue("SAN_API_TOKEN")
	config.FaucetEnabled = envBool("SAN_FAUCET", false)
	config.FaucetAmount = envSANUnits("SAN_FAUCET_AMOUNT", 10.0)
	config.FaucetMax = envSANUnits("SAN_FAUCET_MAX", 100.0)
	config.FaucetCooldown = envFloat("SAN_FAUCET_COOLDOWN", 60.0)
	return config
}

// splitList parses a comma-separated list, dropping empty entries.
func splitList(raw string) []string {
	values := []string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		values = append(values, entry)
	}
	return values
}

// BootstrapAddresses returns the explicit seed list: the comma-separated
// SAN_BOOTSTRAP entries plus the single Bootstrap value when it predates the
// list field (tests and embedders set it directly).
func (config NodeConfig) BootstrapAddresses() []string {
	addresses := append([]string{}, config.BootstrapPeers...)
	if config.Bootstrap != nil && strings.TrimSpace(*config.Bootstrap) != "" {
		for _, entry := range splitList(*config.Bootstrap) {
			duplicate := false
			for _, known := range addresses {
				if known == entry {
					duplicate = true
					break
				}
			}
			if !duplicate {
				addresses = append(addresses, entry)
			}
		}
	}
	return addresses
}

// WideAreaConfigured reports whether DNS seeds or explicit bootstrap seeds are
// configured (the local registry then stays a fallback only).
func (config NodeConfig) WideAreaConfigured() bool {
	return len(config.DNSSeeds) > 0 || len(config.BootstrapAddresses()) > 0
}

// ResolvedPeerCachePath returns SAN_PEERS_CACHE, otherwise
// <db dir>/peers-cache.json, otherwise ~/.san/peers-cache.json.
func (config NodeConfig) ResolvedPeerCachePath() string {
	if path := strings.TrimSpace(config.PeerCachePath); path != "" {
		return path
	}
	if config.DBPath != nil {
		dbPath := strings.TrimSpace(*config.DBPath)
		if dbPath != "" && dbPath != ":memory:" {
			return filepath.Join(filepath.Dir(dbPath), DefaultPeerCacheFile)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".san", DefaultPeerCacheFile)
}

func envStringValue(name string) string {
	value, _ := envString(name)
	return value
}

func optionalEnv(name string) *string {
	if value, ok := envString(name); ok {
		return &value
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ParseGenesisAllocations parses SAN_GENESIS_ALLOCATION entries.
func ParseGenesisAllocations(raw string) map[string]int64 {
	allocations := map[string]int64{}
	if raw == "" {
		return allocations
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		address, amount, found := strings.Cut(entry, ":")
		address = strings.TrimSpace(address)
		amount = strings.TrimSpace(amount)
		if !found || address == "" || amount == "" {
			log.Printf("Ignoring malformed genesis allocation %q", entry)
			continue
		}
		resolved, err := NormalizeAllocationKey(address)
		if err != nil {
			log.Printf("Ignoring genesis allocation %q: %v", entry, err)
			continue
		}
		units, err := ledger.SanToUnits(amount)
		if err != nil {
			log.Printf("Ignoring genesis allocation %q: %v", entry, err)
			continue
		}
		allocations[resolved] = units
	}
	return allocations
}

// ---------------------------------------------------------------------- #
// Peer address helpers
// ---------------------------------------------------------------------- #

// NormalizePeer coerces a peer entry (string or record) into a record dict.
func NormalizePeer(peer any, defaultAPIPort int) (map[string]any, bool) {
	switch value := peer.(type) {
	case string:
		host, port, found := strings.Cut(value, ":")
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, false
		}
		record := map[string]any{"host": host}
		if found && strings.TrimSpace(port) != "" {
			parsed, err := strconv.Atoi(strings.TrimSpace(port))
			if err != nil {
				log.Printf("Invalid peer port in %q", value)
				return nil, false
			}
			record["api_port"] = int64(parsed)
		} else {
			record["api_port"] = int64(defaultAPIPort)
		}
		return record, true
	case map[string]any:
		host := strings.TrimSpace(stringValue(value["host"]))
		if host == "" {
			return nil, false
		}
		record := map[string]any{}
		for key, item := range value {
			record[key] = item
		}
		record["host"] = host
		for _, key := range []string{"api_port", "p2p_port", "peer_port", "controller_port"} {
			if item, ok := record[key]; ok && item != nil {
				switch typed := item.(type) {
				case int:
					record[key] = int64(typed)
				case int64:
				case float64:
					record[key] = int64(typed)
				case string:
					parsed, err := strconv.Atoi(strings.TrimSpace(typed))
					if err != nil {
						log.Printf("Invalid %s in peer %v", key, peer)
						return nil, false
					}
					record[key] = int64(parsed)
				default:
					log.Printf("Invalid %s in peer %v", key, peer)
					return nil, false
				}
			}
		}
		return record, true
	default:
		return nil, false
	}
}

// CompletePeer fills in the local port defaults for missing ports.
func CompletePeer(peer any, config NodeConfig) (map[string]any, bool) {
	record, ok := NormalizePeer(peer, config.APIPort)
	if !ok {
		return nil, false
	}
	if _, exists := record["p2p_port"]; !exists {
		record["p2p_port"] = int64(config.P2PPort)
	}
	if _, exists := record["peer_port"]; !exists {
		record["peer_port"] = int64(config.PeerPort)
	}
	if _, exists := record["controller_port"]; !exists {
		record["controller_port"] = int64(config.ControllerPort)
	}
	return record, true
}

// SamePeer compares two peer records by host and api port.
func SamePeer(a, b map[string]any) bool {
	if a == nil || b == nil {
		return false
	}
	return a["host"] == b["host"] && a["api_port"] == b["api_port"]
}

// PeerAPIURL builds a REST URL for a peer.
func PeerAPIURL(peer map[string]any, path, scheme string) string {
	return scheme + "://" + peerHost(peer) + ":" + peerPort(peer, "api_port") + path
}

// PeerWSURL builds the peer stream URL (kept for compatibility with the old
// WebSocket addressing scheme).
func PeerWSURL(peer map[string]any, portKey string) string {
	scheme := "ws"
	if truthy(peer["tls"]) {
		scheme = "wss"
	}
	return scheme + "://" + peerHost(peer) + ":" + peerPort(peer, portKey)
}

// PeerLabel renders a peer for logs.
func PeerLabel(peer map[string]any) string {
	if peer == nil {
		return "<none>"
	}
	port := "?"
	if value, ok := peer["peer_port"]; ok && value != nil {
		port = stringValue(value)
	}
	return peerHost(peer) + ":" + port
}

func peerHost(peer map[string]any) string { return stringValue(peer["host"]) }

func peerPort(peer map[string]any, key string) string { return stringValue(peer[key]) }

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case int64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return true
	}
}
