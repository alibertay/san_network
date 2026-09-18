// Node is the Go port of network/Node.py: chain state, mempool, P2P
// listeners, consensus, peers and the read API surface used by the HTTP layer.
package netnode

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
	"github.com/alibertay/san_network/internal/sanvm"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// P2P protocol version; a handshake with a different version is rejected.
const ProtocolVersion = 2

// Reject blocks dated more than this far into the future / past (live rules).
const (
	BlockFutureDrift = 120.0
	BlockPastDrift   = 120.0
)

// A peer that answers BLOCK_NOT_FOUND for a hash is not asked again for that
// hash (except as a last resort) for this long.
const BlockMissTTL = 120.0

// Finality vote bounds (mirrors Node.py).
const (
	VoteLookahead          = 64
	MaxStagedVotersPerHash = 128
	VoteMaxBytes           = 16_384
	HelloTTL               = 60.0
)

var (
	voteMetaFields  = []string{"signature", "public_key"}
	peerMetaFields  = []string{"signature"}
	helloMetaFields = []string{"signature"}
)

// Node implements the transport surface.
var _ NodeTransport = (*Node)(nil)

// proposerRoundState mirrors the dict in Node.py: {height, round, started}.
type proposerRoundState struct {
	Height  int64
	Round   int64
	Started time.Time
}

// anchorState is the state the first block of the (possibly pruned) chain
// window starts from.
type anchorState struct {
	Balances     map[string]int64
	Nonces       map[string]int64
	Validators   map[string]map[string]any
	TotalSlashed int64
	TotalBurned  int64
	Parameters   map[string]int64
	BaseFee      int64
	Storage      map[string]any
}

// chainStateSnapshot is the in-memory rollback snapshot for a failed reorg.
type chainStateSnapshot struct {
	Chain         []*ledger.Block
	Balances      map[string]int64
	Nonces        map[string]int64
	Validators    map[string]map[string]any
	TotalSlashed  int64
	TotalBurned   int64
	Parameters    map[string]int64
	BaseFee       int64
	Storage       map[string]any
	ChainHashes   map[string]struct{}
	FinalitySets  map[int64]map[string]int64
	FinalityVotes map[int64]map[string]map[string]map[string]any
	Receipts      map[int64][]any
}

// Node mirrors network/Node.py.
type Node struct {
	config   NodeConfig
	identity *ledger.NodeIdentity
	chainID  string

	// Peer registry.
	PEERS           []map[string]any
	incomingNode    map[string]any
	outgoingNode    map[string]any
	controllerNodes []map[string]any
	seenPeerUpdates map[string]float64
	registry        *PeerRegistry
	syncInFlight    atomic.Bool

	// Local peer management: scores, bans and inbound reservations.
	peerScores     map[string]*peerScoreState
	inboundIPs     map[string]int
	inboundSubnets map[string]int

	// Wide-area discovery: persisted address manager and outbound dialer.
	addrman      *AddrManager
	outboundDial outboundDialFunc

	blockchain    *ledger.Blockchain
	rewardAddress string

	// Finality.
	finalizedHeight      int64
	finalizedHash        string
	finalityVotes        map[int64]map[string]map[string]map[string]any
	finalitySets         map[int64]map[string]int64
	pendingVotes         map[int64]struct{}
	equivocationEvidence []map[string]any
	seenVotes            map[string]struct{}

	// Receipts of recent blocks and operational counters.
	receipts map[int64][]any
	metrics  map[string]int64

	lastSeenBlockIndex int64
	chainHashes        map[string]struct{}
	orphans            map[string]*ledger.Block
	seenBlockGossip    map[string]struct{}
	peerFailures       map[string]int
	requestedBlocks    map[string]struct{}
	peerMissingBlocks  map[string]map[string]float64
	blockMissSyncAt    float64
	syncMisses         int
	proposerState      *proposerRoundState
	anchorState        *anchorState

	storage         *sanvm.Storage
	transactionPool []*ledger.Transaction
	poolTxIDs       map[string]struct{}
	seenTxGossip    map[string]struct{}

	store *ledger.ChainStore

	running   atomic.Bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	servers   []*grpc.Server
	listeners []net.Listener
	stopOnce  sync.Once

	mu        sync.Mutex
	metricsMu sync.Mutex
}

// NewNode builds a node; the constructor never performs network I/O.
func NewNode(config NodeConfig, identity *ledger.NodeIdentity) (*Node, error) {
	return newNode(config, identity, nil)
}

// NewNodeWithStore builds a node on a caller-provided key-value store (tests
// and embedders that need to share an in-memory database).
func NewNodeWithStore(config NodeConfig, identity *ledger.NodeIdentity, kv store.KeyValueStore) (*Node, error) {
	return newNode(config, identity, kv)
}

func newNode(config NodeConfig, identity *ledger.NodeIdentity, kv store.KeyValueStore) (*Node, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config.GenesisAllocations = NormalizeGenesisKeys(config.GenesisAllocations)

	if identity == nil {
		keyFile := ""
		if config.KeyFile != nil {
			keyFile = *config.KeyFile
		}
		loaded, err := ledger.LoadIdentity(keyFile, true)
		if err != nil {
			return nil, err
		}
		identity = loaded
	}

	blockReward, err := ledger.SanToUnits(config.BlockReward)
	if err != nil {
		return nil, fmt.Errorf("invalid block reward: %w", err)
	}
	blockchainConfig := ledger.DefaultBlockchainConfig()
	blockchainConfig.GenesisBalances = config.GenesisAllocations
	blockchainConfig.ChainID = config.ChainID
	blockchainConfig.MinValidatorStake = config.MinValidatorStake
	blockchainConfig.UnbondingPeriod = config.UnbondingPeriod
	blockchainConfig.SlashBps = config.SlashBps
	blockchainConfig.BlockGasLimit = config.BlockGasLimit
	blockchainConfig.ProposerTimeoutMs = int64(config.ProposerTimeout * 1000)
	blockchainConfig.BlockReward = blockReward
	blockchainConfig.MinBlockIntervalMs = config.MinBlockIntervalMs
	blockchain := ledger.NewBlockchain(blockchainConfig)

	rewardAddress := ""
	if config.RewardAddress != "" {
		normalized, err := ledger.NormalizeAddress(config.RewardAddress)
		if err != nil {
			return nil, fmt.Errorf("Invalid SAN_REWARD_ADDRESS %q: %v", config.RewardAddress, err)
		}
		rewardAddress = normalized
	}

	node := &Node{
		config:               config,
		identity:             identity,
		chainID:              config.ChainID,
		PEERS:                []map[string]any{},
		seenPeerUpdates:      map[string]float64{},
		blockchain:           blockchain,
		rewardAddress:        rewardAddress,
		finalizedHeight:      0,
		finalizedHash:        blockchain.Tip().CurrentBlockHash,
		finalityVotes:        map[int64]map[string]map[string]map[string]any{},
		finalitySets:         map[int64]map[string]int64{},
		pendingVotes:         map[int64]struct{}{},
		equivocationEvidence: []map[string]any{},
		seenVotes:            map[string]struct{}{},
		receipts:             map[int64][]any{},
		metrics: map[string]int64{
			"blocks_committed":           0,
			"transactions_committed":     0,
			"votes_received":             0,
			"votes_seen":                 0,
			"vote_messages_received":     0,
			"votes_dropped_invalid":      0,
			"votes_dropped_height":       0,
			"votes_dropped_voter":        0,
			"votes_dropped_duplicate":    0,
			"votes_dropped_equivocation": 0,
			"votes_broadcast":            0,
			"votes_unbroadcast":          0,
			"reorgs":                     0,
			"slashing_events":            0,
			"governance_changes":         0,
			"peer_penalties":             0,
			"peers_banned":               0,
			"peers_rejected_inbound":     0,
			"peers_rejected_subnet":      0,
			"peers_rejected_banned":      0,
			"peer_malformed_messages":    0,
			"peer_rate_limit_hits":       0,
			"peer_invalid_records":       0,
		},
		lastSeenBlockIndex: blockchain.Tip().Index,
		chainHashes:        map[string]struct{}{},
		orphans:            map[string]*ledger.Block{},
		seenBlockGossip:    map[string]struct{}{},
		peerFailures:       map[string]int{},
		requestedBlocks:    map[string]struct{}{},
		peerMissingBlocks:  map[string]map[string]float64{},
		storage:            sanvm.NewStorage(),
		transactionPool:    []*ledger.Transaction{},
		poolTxIDs:          map[string]struct{}{},
		seenTxGossip:       map[string]struct{}{},
		peerScores:         map[string]*peerScoreState{},
		inboundIPs:         map[string]int{},
		inboundSubnets:     map[string]int{},
	}
	for _, block := range blockchain.Chain {
		node.chainHashes[block.CurrentBlockHash] = struct{}{}
	}

	if config.SnapshotInterval > 0 && config.PruneKeep > 0 && config.PruneKeep < config.SnapshotInterval {
		log.Printf("SAN_PRUNE_KEEP (%d) is below SAN_SNAPSHOT_INTERVAL (%d); pruning will be limited to the newest snapshot height",
			config.PruneKeep, config.SnapshotInterval)
	}

	if kv != nil {
		chainStore, err := ledger.NewChainStore(":memory:", "memory", kv)
		if err != nil {
			return nil, err
		}
		node.store = chainStore
	} else if config.DBPath != nil {
		chainStore, err := ledger.NewChainStore(*config.DBPath, config.DBBackend, nil)
		if err != nil {
			return nil, err
		}
		node.store = chainStore
	}

	if node.store != nil {
		if node.store.IsEmpty() {
			zero := int64(0)
			if err := node.store.AppendBlock(node.blockchain.Tip(), ledger.AppendOptions{
				Receipts: []any{},
				State:    node.blockchain.StateSnapshot(),
				Storage:  node.storage.ToDict(),
				Head:     &zero,
			}); err != nil {
				return nil, err
			}
			if err := node.store.SetMeta("genesis_allocation", node.genesisAllocationFingerprint()); err != nil {
				return nil, err
			}
		} else {
			if err := node.loadPersistedState(node.store); err != nil {
				return nil, err
			}
		}
	}
	return node, nil
}

// ---------------------------------------------------------------------- #
// Lifecycle
// ---------------------------------------------------------------------- #

// Start starts the gRPC servers and the background loops. It mirrors
// Node.start(): the caller must Stop() the node.
func (n *Node) Start(ctx context.Context) error {
	if n.running.Load() {
		return nil
	}
	n.running.Store(true)
	runCtx, cancel := context.WithCancel(ctx)
	n.cancel = cancel

	if err := n.startServers(); err != nil {
		n.running.Store(false)
		cancel()
		return err
	}
	if n.discoveryActive() {
		n.startDiscovery(runCtx)
	}
	n.bootstrap(runCtx)
	n.refreshPeerSelection()
	if n.config.PublicDevnet {
		n.mu.Lock()
		controllers := len(n.controllerNodes)
		n.mu.Unlock()
		if controllers == 0 {
			log.Printf("WARNING: public devnet mode is enabled but the effective controller set is empty; "+
				"block production will not be pre-committed until %d controller(s) are discovered (check SAN_DNS_SEEDS/SAN_BOOTSTRAP)",
				n.config.ControllerCount)
		}
	}
	n.wg.Add(2)
	go func() { defer n.wg.Done(); n.peerHealthLoop(runCtx) }()
	go func() { defer n.wg.Done(); n.blockProductionLoop(runCtx) }()

	n.mu.Lock()
	peerCount := len(n.PEERS)
	n.mu.Unlock()
	if peerCount > 0 {
		n.Synchronize(runCtx)
		if n.PendingCount() == 0 {
			n.RequestMempool(runCtx, nil)
		}
	}

	log.Printf("Node started (api=%d p2p=%d peer=%d controller=%d peers=%d tls=%v db=%v)",
		n.config.APIPort, n.config.P2PPort, n.config.PeerPort, n.config.ControllerPort,
		peerCount, n.config.TLSEnabled(), n.store != nil)
	return nil
}

// Stop tears down the servers, loops and the store.
func (n *Node) Stop() {
	n.running.Store(false)
	if n.cancel != nil {
		n.cancel()
		n.cancel = nil
	}
	n.wg.Wait()
	if n.registry != nil {
		registry := n.registry
		n.registry = nil
		if err := registry.Remove(n.identity.PublicKeyHex(), "", 0); err != nil {
			log.Printf("Cannot remove the peer registry entry: %v", err)
		}
	}
	if n.addrman != nil {
		n.saveAddrman()
	}
	for _, server := range n.servers {
		server.Stop()
	}
	n.servers = nil
	n.listeners = nil
	n.stopOnce.Do(func() {
		if n.store != nil {
			_ = n.store.Close()
		}
	})
}

func (n *Node) startServers() error {
	server := BuildServer(n, n.config)
	ports := []int{n.config.PeerPort, n.config.P2PPort, n.config.ControllerPort}
	for _, port := range ports {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", n.config.Host, port))
		if err != nil {
			for _, started := range n.listeners {
				_ = started.Close()
			}
			n.listeners = nil
			server.Stop()
			return err
		}
		n.listeners = append(n.listeners, listener)
	}
	n.servers = append(n.servers, server)
	for _, listener := range n.listeners {
		go func(listener net.Listener) { _ = server.Serve(listener) }(listener)
	}
	return nil
}

// ---------------------------------------------------------------------- #
// TLS
// ---------------------------------------------------------------------- #

// TransportServerCredentials loads the configured certificate pair.
func (n *Node) TransportServerCredentials() credentials.TransportCredentials {
	if !n.config.TLSEnabled() {
		log.Printf("TLS is disabled: P2P traffic is plaintext. Set SAN_TLS_CERT/SAN_TLS_KEY (and SAN_TLS_CA on peers) for production.")
		return nil
	}
	certificate, err := tls.LoadX509KeyPair(*n.config.TLSCert, *n.config.TLSKey)
	if err != nil {
		log.Printf("Cannot load TLS certificate: %v", err)
		return nil
	}
	return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{certificate}})
}

// TransportClientCredentials loads SAN_TLS_CA when configured; without it the
// system trust store is used, exactly like Python's ssl_channel_credentials().
func (n *Node) TransportClientCredentials(peer map[string]any) credentials.TransportCredentials {
	if n.config.TLSCA == nil || strings.TrimSpace(*n.config.TLSCA) == "" {
		log.Printf("SAN_TLS_CA is not configured; the system trust store is used for peer certificates")
		return credentials.NewTLS(&tls.Config{})
	}
	ca, err := os.ReadFile(*n.config.TLSCA)
	if err != nil {
		log.Printf("Cannot read TLS CA %s: %v", *n.config.TLSCA, err)
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		log.Printf("Invalid TLS CA bundle %s", *n.config.TLSCA)
		return nil
	}
	return credentials.NewTLS(&tls.Config{RootCAs: pool})
}

// ---------------------------------------------------------------------- #
// Protocol handshake
// ---------------------------------------------------------------------- #

// HelloPayload builds the signed hello record for a message type.
func (n *Node) HelloPayload(messageType string) map[string]any {
	record := map[string]any{
		"type":       messageType,
		"protocol":   int64(ProtocolVersion),
		"chain_id":   n.chainID,
		"public_key": n.GetPublicKey(),
		"timestamp":  nowSeconds(),
	}
	if encoded, err := canonical.Marshal(record); err == nil {
		if signature := n.identity.SignHex(encoded); signature != "" {
			record["signature"] = signature
		}
	}
	return record
}

// VerifyHello validates a HELLO/HELLO_ACK record.
func (n *Node) VerifyHello(data map[string]any) bool {
	if data == nil {
		return false
	}
	messageType, _ := data["type"].(string)
	if messageType != "HELLO" && messageType != "HELLO_ACK" {
		return false
	}
	if int64Value(data["protocol"]) != ProtocolVersion {
		log.Printf("Handshake rejected: protocol %v", data["protocol"])
		return false
	}
	if chainID, _ := data["chain_id"].(string); chainID != n.chainID {
		log.Printf("Handshake rejected: chain_id %v does not match %q", data["chain_id"], n.chainID)
		return false
	}
	timestamp, ok := numericValue(data["timestamp"])
	if !ok {
		return false
	}
	if math.Abs(nowSeconds()-timestamp) > HelloTTL {
		log.Printf("Handshake rejected: stale timestamp")
		return false
	}
	publicKey, _ := data["public_key"].(string)
	signature, _ := data["signature"].(string)
	if publicKey == "" || signature == "" {
		return false
	}
	payload := mapWithout(data, helloMetaFields)
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return false
	}
	return ledger.VerifyIdentity(encoded, signature, publicKey)
}

// ---------------------------------------------------------------------- #
// Identity, signing and votes
// ---------------------------------------------------------------------- #

// GetPublicKey returns the node public key as hex ("" when not configured).
func (n *Node) GetPublicKey() string {
	return n.identity.PublicKeyHex()
}

// SignBlockHash signs a block hash and returns the hex signature ("" without
// a private key).
func (n *Node) SignBlockHash(blockHash string) string {
	signature := n.identity.SignHex([]byte(blockHash))
	if signature == "" {
		log.Printf("No private key configured; producing an unsigned block")
	}
	return signature
}

// VerifyBlockSignature checks the validator signature over the block hash.
func (n *Node) VerifyBlockSignature(block *ledger.Block) bool {
	if !truthy(block.ValidatorSignature) {
		return !n.config.RequireBlockSig
	}
	if !truthy(block.Validator) {
		return false
	}
	return ledger.VerifyIdentity(
		[]byte(block.CurrentBlockHash),
		stringValue(block.ValidatorSignature),
		stringValue(block.Validator),
	)
}

// SignVote signs the response payload (excluding signature/public_key).
func (n *Node) SignVote(response map[string]any) string {
	payload := mapWithout(response, voteMetaFields)
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return ""
	}
	return n.identity.SignHex(encoded)
}

// ---------------------------------------------------------------------- #
// Read-only queries
// ---------------------------------------------------------------------- #

// BlockAt returns the block at an absolute height, honouring a pruned window.
func (n *Node) BlockAt(height int64) *ledger.Block {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blockAt(height)
}

func (n *Node) blockAt(height int64) *ledger.Block {
	chain := n.blockchain.Chain
	if len(chain) == 0 {
		return nil
	}
	offset := height - chain[0].Index
	if offset < 0 || offset >= int64(len(chain)) {
		return nil
	}
	return chain[offset]
}

// BlockHashAt returns the hash at a height, or "".
func (n *Node) BlockHashAt(height int64) string {
	block := n.BlockAt(height)
	if block == nil {
		return ""
	}
	return block.CurrentBlockHash
}

// Config returns the node configuration.
func (n *Node) Config() NodeConfig { return n.config }

// Identity returns the node identity.
func (n *Node) Identity() *ledger.NodeIdentity { return n.identity }

// ChainID returns the chain id.
func (n *Node) ChainID() string { return n.chainID }

// Tip returns the current chain tip.
func (n *Node) Tip() *ledger.Block {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blockchain.Tip()
}

// FinalizedHeight returns the finality checkpoint height.
func (n *Node) FinalizedHeight() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.finalizedHeight
}

// FinalizedHash returns the finality checkpoint hash.
func (n *Node) FinalizedHash() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.finalizedHash
}

// GetAccount returns the account balance and nonce.
func (n *Node) GetAccount(address string) (map[string]any, error) {
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	units := n.blockchain.SAN[normalized]
	return map[string]any{
		"address":       normalized,
		"balance_units": units,
		"balance":       ledger.UnitsToSAN(units),
		"nonce":         n.blockchain.Nonces[normalized],
	}, nil
}

// GetBlock returns the full block dict at a height, or nil.
func (n *Node) GetBlock(index int64) map[string]any {
	block := n.BlockAt(index)
	if block == nil {
		return nil
	}
	return block.ToDict()
}

// ListContracts returns the sorted deployed contract ids.
func (n *Node) ListContracts() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := make([]string, 0, len(n.storage.Contracts))
	for id := range n.storage.Contracts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// QueryContract executes a read-only contract call on a sandboxed copy.
func (n *Node) QueryContract(contractID, functionName string, params []any) (any, error) {
	n.mu.Lock()
	contract, ok := n.storage.Contracts[contractID]
	var copied map[string]any
	if ok {
		copied = deepCopyStringMap(contract)
	}
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("Unknown contract: %s", contractID)
	}
	sandbox := sanvm.NewStorage()
	sandbox.Contracts[contractID] = copied
	vm := sanvm.NewVM(sandbox)
	return vm.CallContractFunction(contractID, functionName, params, nil)
}

// CurrentStateRoot recomputes the ledger state root.
func (n *Node) CurrentStateRoot() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentStateRoot()
}

func (n *Node) currentStateRoot() string {
	return ledger.StateRoot(ledger.StateInput{
		Balances:     n.blockchain.SAN,
		Nonces:       n.blockchain.Nonces,
		Validators:   n.blockchain.Validators,
		TotalSlashed: n.blockchain.TotalSlashed,
		Storage:      n.storage.ToDict(),
		Parameters:   n.blockchain.Parameters,
		BaseFee:      n.blockchain.BaseFee,
		TotalBurned:  n.blockchain.TotalBurned,
	})
}

// GetHeaders returns light-client headers.
func (n *Node) GetHeaders(fromIndex int64, limit int) []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	limitValue := int64(limit)
	blocks := n.blockchain.BlocksSince(fromIndex, &limitValue)
	headers := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		headers = append(headers, block.ToHeaderDict())
	}
	return headers
}

// GetAccountProof returns the Merkle proof of an account against the state.
func (n *Node) GetAccountProof(address string) (map[string]any, error) {
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	proof := ledger.StateEntryProof(n.stateInput(), ledger.AccountKey(normalized))
	if proof == nil {
		return nil, nil
	}
	proof["address"] = normalized
	return proof, nil
}

func (n *Node) stateInput() ledger.StateInput {
	return ledger.StateInput{
		Balances:     n.blockchain.SAN,
		Nonces:       n.blockchain.Nonces,
		Validators:   n.blockchain.Validators,
		TotalSlashed: n.blockchain.TotalSlashed,
		Storage:      n.storage.ToDict(),
		Parameters:   n.blockchain.Parameters,
		BaseFee:      n.blockchain.BaseFee,
		TotalBurned:  n.blockchain.TotalBurned,
	}
}

// GetTransactionProof returns the Merkle inclusion proof of a transaction.
func (n *Node) GetTransactionProof(blockIndex, txIndex int64) map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	block := n.blockAt(blockIndex)
	if block == nil {
		return nil
	}
	entries := make([]any, len(block.Transactions))
	for i, tx := range block.Transactions {
		entries[i] = transactionDict(tx)
	}
	if txIndex < 0 || txIndex >= int64(len(entries)) {
		return nil
	}
	proof, err := ledger.MerkleProof(entries, int(txIndex))
	if err != nil {
		return nil
	}
	return map[string]any{
		"block_index": blockIndex,
		"block_hash":  block.CurrentBlockHash,
		"tx_root":     block.TxRoot,
		"tx_index":    txIndex,
		"transaction": entries[txIndex],
		"proof":       proof,
	}
}

// FinalizedSnapshot returns the latest safe state snapshot, or nil.
func (n *Node) FinalizedSnapshot() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.store == nil {
		return nil
	}
	snapshots, err := n.store.ListSnapshots()
	if err != nil {
		return nil
	}
	var snapshot map[string]any
	found := false
	for _, candidate := range snapshots {
		if int64Value(candidate["height"]) > n.finalizedHeight {
			continue
		}
		block := n.blockAt(int64Value(candidate["height"]))
		if block == nil || block.CurrentBlockHash != stringValue(candidate["hash"]) {
			continue
		}
		snapshot = candidate
		found = true
		break
	}
	if !found {
		return nil
	}
	return map[string]any{
		"chain_id":   n.chainID,
		"height":     snapshot["height"],
		"block_hash": snapshot["hash"],
		"state_root": n.blockAt(int64Value(snapshot["height"])).StateRoot,
		"state":      snapshot["state"],
		"storage":    snapshot["storage"],
	}
}

// GetReceipt returns the execution receipt of a recent block.
func (n *Node) GetReceipt(blockIndex, txIndex int64) map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	receipts, ok := n.receipts[blockIndex]
	if !ok && n.store != nil {
		stored, present, err := n.store.ReceiptsForBlock(blockIndex)
		if err == nil && present {
			receipts = stored
			ok = true
		}
	}
	if !ok || txIndex < 0 || txIndex >= int64(len(receipts)) {
		return nil
	}
	receipt, _ := receipts[txIndex].(map[string]any)
	result := map[string]any{"block_index": blockIndex, "block_hash": nil}
	if block := n.blockAt(blockIndex); block != nil {
		result["block_hash"] = block.CurrentBlockHash
	}
	for key, value := range receipt {
		result[key] = value
	}
	return result
}

// GetTransaction resolves a transaction id through the tx index (or the
// in-memory chain when there is no database).
func (n *Node) GetTransaction(txID string) map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.store != nil {
		location, ok, err := n.store.TxLookup(txID)
		if err != nil || !ok {
			return nil
		}
		block := n.blockAt(int64Value(location["block_index"]))
		if block == nil {
			return nil
		}
		if stringValue(location["block_hash"]) != block.CurrentBlockHash {
			log.Printf("Stale tx index entry for %s", txID)
			return nil
		}
		txIndex := int(int64Value(location["tx_index"]))
		if txIndex < 0 || txIndex >= len(block.Transactions) {
			return nil
		}
		txMap := transactionDict(block.Transactions[txIndex])
		txDict, _ := txMap.(map[string]any)
		if txDict == nil || ledger.TransactionID(txDict) != txID {
			return nil
		}
		return n.transactionResult(txID, block, txIndex, txDict)
	}

	chain := n.blockchain.Chain
	for blockIndex := len(chain) - 1; blockIndex >= 0; blockIndex-- {
		block := chain[blockIndex]
		for index, raw := range block.Transactions {
			txMap := transactionDict(raw)
			txDict, _ := txMap.(map[string]any)
			if txDict == nil {
				continue
			}
			if ledger.TransactionID(txDict) == txID {
				return n.transactionResult(txID, block, index, txDict)
			}
		}
	}
	return nil
}

func (n *Node) transactionResult(txID string, block *ledger.Block, txIndex int, txDict map[string]any) map[string]any {
	return map[string]any{
		"tx_id":       txID,
		"block_index": block.Index,
		"block_hash":  block.CurrentBlockHash,
		"tx_index":    int64(txIndex),
		"transaction": txDict,
		"receipt":     n.getReceiptLocked(block.Index, int64(txIndex)),
	}
}

func (n *Node) getReceiptLocked(blockIndex, txIndex int64) map[string]any {
	receipts, ok := n.receipts[blockIndex]
	if !ok && n.store != nil {
		stored, present, err := n.store.ReceiptsForBlock(blockIndex)
		if err == nil && present {
			receipts = stored
			ok = true
		}
	}
	if !ok || txIndex < 0 || txIndex >= int64(len(receipts)) {
		return nil
	}
	receipt, _ := receipts[txIndex].(map[string]any)
	result := map[string]any{"block_index": blockIndex, "block_hash": nil}
	if block := n.blockAt(blockIndex); block != nil {
		result["block_hash"] = block.CurrentBlockHash
	}
	for key, value := range receipt {
		result[key] = value
	}
	return result
}

// MetricsSnapshot exports counters and gauges for /metrics.
func (n *Node) MetricsSnapshot() map[string]any {
	n.mu.Lock()
	height := n.blockchain.Tip().Index
	active := n.blockchain.ActiveValidators()
	totalStake := int64(0)
	for _, stake := range active {
		totalStake += stake
	}
	result := map[string]any{
		"height":             height,
		"finalized_height":   n.finalizedHeight,
		"peers":              int64(len(n.PEERS)),
		"controllers":        int64(len(n.controllerNodes)),
		"controllers_target": int64(n.config.ControllerCount),
		"mempool":            int64(len(n.transactionPool)),
		"validators":         int64(len(active)),
		"total_stake_units":  totalStake,
		"total_slashed":      n.blockchain.TotalSlashed,
		"total_burned":       n.blockchain.TotalBurned,
		"base_fee":           n.blockchain.BaseFee,
		"contracts":          int64(len(n.storage.Contracts)),
		"orphans":            int64(len(n.orphans)),
		"peer_bans_active":   n.activePeerBansLocked(),
	}
	n.mu.Unlock()

	n.metricsMu.Lock()
	for key, value := range n.metrics {
		result[key] = value
	}
	n.metricsMu.Unlock()
	return result
}

// PendingCount returns the mempool size.
func (n *Node) PendingCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.transactionPool)
}

// PendingTxIDs returns the mempool transaction ids.
func (n *Node) PendingTxIDs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := make([]string, 0, len(n.transactionPool))
	for _, tx := range n.transactionPool {
		ids = append(ids, ledger.TxID(tx.Payload))
	}
	return ids
}

// ---------------------------------------------------------------------- #
// Helpers
// ---------------------------------------------------------------------- #

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func timeNow() time.Time { return time.Now() }

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }

// decodeObject parses a wire message with Python json.loads semantics
// (canonical ints stay ints, floats stay floats).
func decodeObject(raw string) (map[string]any, error) {
	value, err := canonical.Decode([]byte(raw))
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return object, nil
}

// encodeObject serializes a wire message the way Python json.dumps does
// (floats keep their Python representation, so hashes and signatures match).
func encodeObject(value any) ([]byte, error) {
	return canonical.Marshal(value)
}

func (n *Node) sessionTimeout() time.Duration {
	return time.Duration(n.config.WSTimeout * float64(time.Second))
}

func (n *Node) incMetric(name string) {
	n.metricsMu.Lock()
	n.metrics[name]++
	n.metricsMu.Unlock()
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		return int64(typed)
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed
	default:
		return 0
	}
}

// int64Strict is Python's isinstance(value, int) (bools rejected).
func int64Strict(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func mapWithout(value map[string]any, excluded []string) map[string]any {
	result := map[string]any{}
	for key, item := range value {
		skip := false
		for _, name := range excluded {
			if key == name {
				skip = true
				break
			}
		}
		if !skip {
			result[key] = item
		}
	}
	return result
}

func intMapToAny(input map[string]int64) map[string]any {
	result := map[string]any{}
	for key, value := range input {
		result[key] = value
	}
	return result
}

func intMapFromAny(value any) map[string]int64 {
	result := map[string]int64{}
	raw, ok := value.(map[string]any)
	if !ok {
		return result
	}
	for key, item := range raw {
		result[key] = int64Value(item)
	}
	return result
}

func validatorsFromAny(value any) map[string]map[string]any {
	result := map[string]map[string]any{}
	raw, ok := value.(map[string]any)
	if !ok {
		return result
	}
	for address, item := range raw {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		result[address] = deepCopyStringMap(record)
	}
	return result
}

func copyIntMap(input map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func copyValidators(input map[string]map[string]any) map[string]map[string]any {
	result := make(map[string]map[string]any, len(input))
	for address, info := range input {
		result[address] = deepCopyStringMap(info)
	}
	return result
}

func copyStringSet(input map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(input))
	for key := range input {
		result[key] = struct{}{}
	}
	return result
}

func copyFinalitySets(input map[int64]map[string]int64) map[int64]map[string]int64 {
	result := make(map[int64]map[string]int64, len(input))
	for height, weights := range input {
		result[height] = copyIntMap(weights)
	}
	return result
}

func copyFinalityVotes(input map[int64]map[string]map[string]map[string]any) map[int64]map[string]map[string]map[string]any {
	result := make(map[int64]map[string]map[string]map[string]any, len(input))
	for height, hashes := range input {
		hashCopy := make(map[string]map[string]map[string]any, len(hashes))
		for blockHash, voters := range hashes {
			voterCopy := make(map[string]map[string]any, len(voters))
			for address, vote := range voters {
				voterCopy[address] = deepCopyStringMap(vote)
			}
			hashCopy[blockHash] = voterCopy
		}
		result[height] = hashCopy
	}
	return result
}

func deepCopyStringMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = deepCopyAny(value)
	}
	return result
}

func deepCopyAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyStringMap(typed)
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = deepCopyAny(item)
		}
		return result
	default:
		return typed
	}
}

func copyReceipts(input map[int64][]any) map[int64][]any {
	result := make(map[int64][]any, len(input))
	for height, receipts := range input {
		result[height] = append([]any{}, receipts...)
	}
	return result
}

// transactionDict mirrors Block._transaction_to_dict for netnode purposes.
func transactionDict(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case *ledger.Transaction:
		return typed.Payload
	case ledger.Transaction:
		return typed.Payload
	default:
		return map[string]any{"data": fmt.Sprintf("%v", value)}
	}
}

func txPayloadMap(value any) (map[string]any, bool) {
	converted := transactionDict(value)
	result, ok := converted.(map[string]any)
	return result, ok
}

func sortedInt64Keys(input map[int64]map[string]map[string]map[string]any) []int64 {
	keys := make([]int64, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func sortedStringMapKeys(input map[string]int64) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// GetLocalIP guesses the outbound local address (Python's get_local_ip).
func (n *Node) GetLocalIP() string {
	connection, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer connection.Close()
		if address, ok := connection.LocalAddr().(*net.UDPAddr); ok {
			return address.IP.String()
		}
	}
	hostname, err := os.Hostname()
	if err == nil {
		addresses, err := net.LookupHost(hostname)
		if err == nil && len(addresses) > 0 {
			return addresses[0]
		}
	}
	return "127.0.0.1"
}

// DiscoverPeers fetches the peer list from the bootstrap node (best effort).
// TLS-enabled nodes serve their REST API over HTTPS, so the fallback path
// (used by the same-machine port probe and seed bootstrap) picks the matching
// scheme and trusts the configured devnet CA.
func (n *Node) DiscoverPeers(bootstrapNode string) []any {
	baseURL := bootstrapNode
	if !strings.Contains(bootstrapNode, "://") {
		scheme := "http"
		if n.config.TLSEnabled() {
			scheme = "https"
		}
		baseURL = scheme + "://" + bootstrapNode
	}
	client := &http.Client{Timeout: 5 * time.Second}
	if strings.HasPrefix(baseURL, "https://") {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
		if n.config.TLSCA != nil {
			if pem, err := os.ReadFile(*n.config.TLSCA); err == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(pem) {
					tlsConfig.RootCAs = pool
					tlsConfig.InsecureSkipVerify = false
				}
			}
		}
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	response, err := client.Get(strings.TrimRight(baseURL, "/") + "/bootstrap")
	if err != nil {
		log.Printf("Could not fetch peers from %s: %v", bootstrapNode, err)
		return []any{}
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		log.Printf("Could not fetch peers from %s: %v", bootstrapNode, err)
		return []any{}
	}
	peers, _ := payload["peers"].([]any)
	if peers == nil {
		return []any{}
	}
	return peers
}
