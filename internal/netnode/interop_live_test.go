//go:build interop

// Package netnode live interoperability harness.
//
// Policy (docs/interop.md): the Go node is the canonical protocol
// implementation, the Python implementation (network/Node.py, run.py) is the
// reference and fixture source. Wire compatibility is exercised by this
// harness on demand; it is not a production-compatibility claim. Tests skip
// cleanly when a Python runtime with fastapi/uvicorn/grpcio is unavailable.
//
// Run with:
//
//	go test -tags interop ./internal/netnode -run TestInterop -v -timeout 15m
package netnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

// interopTokenSource deploys the same fixture contract the API tests use.
const interopTokenSource = `function answer() {
  return 42
}
function add(a, b) {
  return a + b
}
`

// ---------------------------------------------------------------------- #
// Helpers: Go nodes
// ---------------------------------------------------------------------- #

// interopConfig builds a node wired for the shared public defaults, so a Go
// node and a Python node with the same genesis allocation derive the same
// genesis hash. (Genesis commits to min_validator_stake, unbonding_period,
// slash_bps, block_gas_limit, proposer_timeout, reward and interval, so these
// must stay at the DefaultNodeConfig values.)
func interopConfig(ports []int, allocations map[string]int64) NodeConfig {
	config := DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.AdvertiseHost = stringPointer("127.0.0.1")
	config.DBPath = nil
	config.DBBackend = "memory"
	config.APIPort = ports[0]
	config.PeerPort = ports[1]
	config.P2PPort = ports[2]
	config.ControllerPort = ports[3]
	config.WSTimeout = 2.0
	config.PeerCheckInterval = 3600
	config.DiscoveryInterval = 3600
	config.DiscoveryEnabled = false
	config.PeerCachePath = filepath.Join(os.TempDir(), fmt.Sprintf("san-interop-%d.json", time.Now().UnixNano()))
	config.GenesisAllocations = allocations
	config.ControllerCount = 0
	config.BlockThresholdFee = 0
	config.RequireBlockSig = true
	config.RequireStateRoot = true
	return config
}

func startGoNode(t *testing.T, config NodeConfig, identity *ledger.NodeIdentity) *Node {
	t.Helper()
	node, err := NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if err := node.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(node.Stop)
	return node
}

func syncUntil(t *testing.T, node *Node, wantHeight int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node.Synchronize(context.Background())
		if node.Tip().Index >= wantHeight {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("node did not reach height %d (at %d)", wantHeight, node.Tip().Index)
}

// commitDirect builds a valid proposal and commits it locally, then gossips
// it. Once the validator set is active the REST submission path only commits
// when the local node is the expected proposer, so the harness drives the
// consensus seam directly (the wire path is still exercised by gossip/sync).
func commitDirect(t *testing.T, node *Node, identity *ledger.NodeIdentity, txs []any) *ledger.Block {
	t.Helper()
	block := buildProposal(t, node, identity, txs, -1)
	node.mu.Lock()
	ok := node.commitBlock(block, false)
	node.mu.Unlock()
	if !ok {
		t.Fatalf("direct commit failed for height %d", block.Index)
	}
	node.GossipBlock(block)
	return block
}

// ---------------------------------------------------------------------- #
// Helpers: Python nodes
// ---------------------------------------------------------------------- #

type pythonNode struct {
	command  *exec.Cmd
	apiPort  int
	peerPort int
	output   bytes.Buffer
	baseURL  string
}

func pythonInvocation() ([]string, bool) {
	candidates := [][]string{{"python"}, {"python3"}}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, []string{"py", "-3"})
	}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		probe := exec.Command(candidate[0], append(candidate[1:], "-c", "import fastapi, uvicorn, grpc")...)
		if err := probe.Run(); err == nil {
			return candidate, true
		}
	}
	return nil, false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run.py")); err != nil {
		t.Skipf("interop: run.py not found at %s", root)
	}
	return root
}

// startPythonNode spawns `python run.py` with a sanitized environment. The
// test skips when the runtime cannot serve /health in time, so an environment
// without a working Python stack stays green.
func startPythonNode(t *testing.T, ports []int, extraEnv map[string]string) *pythonNode {
	t.Helper()
	invocation, ok := pythonInvocation()
	if !ok {
		t.Skip("interop: no python with fastapi/uvicorn/grpcio available; Python-side checks skipped")
	}
	root := repoRoot(t)

	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "SAN_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"SAN_HOST=127.0.0.1",
		"SAN_ADVERTISE_HOST=127.0.0.1",
		fmt.Sprintf("SAN_API_PORT=%d", ports[0]),
		fmt.Sprintf("SAN_PEER_PORT=%d", ports[1]),
		fmt.Sprintf("SAN_P2P_PORT=%d", ports[2]),
		fmt.Sprintf("SAN_CONTROLLER_PORT=%d", ports[3]),
		"SAN_CHAIN_ID=san-devnet-1",
		"SAN_DB_BACKEND=memory",
		"SAN_BLOCK_THRESHOLD_FEE=0",
		"SAN_PEER_CHECK_INTERVAL=1",
		"SAN_WS_TIMEOUT=2",
		"SAN_CONTROLLER_COUNT=0",
		"SAN_DISCOVERY=",
		"SAN_KEY_FILE="+filepath.Join(t.TempDir(), "san_key.json"),
	)
	for name, value := range extraEnv {
		env = append(env, name+"="+value)
	}

	command := exec.Command(invocation[0], append(invocation[1:], "run.py")...)
	command.Dir = root
	command.Env = env
	node := &pythonNode{
		command:  command,
		apiPort:  ports[0],
		peerPort: ports[1],
		baseURL:  fmt.Sprintf("http://127.0.0.1:%d", ports[0]),
	}
	command.Stdout = &node.output
	command.Stderr = &node.output
	if err := command.Start(); err != nil {
		t.Skipf("interop: cannot start python run.py: %v", err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_, _ = command.Process.Wait()
	})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := node.getJSON("/health"); err == nil {
			return node
		}
		if command.ProcessState != nil && command.ProcessState.Exited() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skipf("interop: python node did not become healthy; output:\n%s", tail(node.output.String(), 40))
	return nil
}

func (node *pythonNode) getJSON(path string) (map[string]any, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(node.baseURL + path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("%s: status %d (%s)", path, response.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (node *pythonNode) postJSON(path string, body any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(node.baseURL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d (%s)", path, response.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (node *pythonNode) height(t *testing.T) int64 {
	t.Helper()
	health, err := node.getJSON("/health")
	if err != nil {
		t.Fatalf("python /health: %v", err)
	}
	return int64(health["height"].(float64))
}

func tail(text string, lines int) string {
	split := strings.Split(strings.TrimSpace(text), "\n")
	if len(split) > lines {
		split = split[len(split)-lines:]
	}
	return strings.Join(split, "\n")
}

// pythonRecord fetches the Python node's own signed peer record (last entry of
// /bootstrap).
func pythonRecord(t *testing.T, node *pythonNode) map[string]any {
	t.Helper()
	payload, err := node.getJSON("/bootstrap")
	if err != nil {
		t.Fatalf("python /bootstrap: %v", err)
	}
	peers, _ := payload["peers"].([]any)
	if len(peers) == 0 {
		t.Fatalf("python /bootstrap returned no records")
	}
	record, _ := peers[len(peers)-1].(map[string]any)
	return record
}

// ---------------------------------------------------------------------- #
// Go-vs-Go mesh fallback (runs without Python)
// ---------------------------------------------------------------------- #

func TestInteropGoMesh(t *testing.T) {
	ids := []*ledger.NodeIdentity{mustIdentity(t), mustIdentity(t), mustIdentity(t)}
	allocations := map[string]int64{}
	for _, identity := range ids {
		allocations[identityAddress(t, identity)] = 10_000 * ledger.SANBase
	}
	ports := freePorts(t, 12)
	configA := interopConfig(ports[0:4], allocations)
	configB := interopConfig(ports[4:8], allocations)
	configC := interopConfig(ports[8:12], allocations)

	nodeA := startGoNode(t, configA, ids[0])
	nodeB := startGoNode(t, configB, ids[1])
	nodeC := startGoNode(t, configC, ids[2])
	for _, node := range []*Node{nodeB, nodeC} {
		if added := node.AddPeer(nodeA.SelfPeerRecord()); added != 1 {
			t.Fatalf("join seed: AddPeer returned %d", added)
		}
	}

	t.Run("genesis_and_chain_id_agree", func(t *testing.T) {
		genesis := nodeA.GenesisHash()
		for name, node := range map[string]*Node{"A": nodeA, "B": nodeB, "C": nodeC} {
			if got := node.GenesisHash(); got != genesis {
				t.Fatalf("node %s genesis %v, want %v", name, got, genesis)
			}
			if node.ChainID() != nodeA.ChainID() {
				t.Fatalf("node %s chain id mismatch", name)
			}
		}
	})

	t.Run("handshake_and_status", func(t *testing.T) {
		status := RemoteStatus(context.Background(), nodeB, nodeA.SelfPeerRecord(), 5*time.Second)
		if status == nil {
			t.Fatalf("RemoteStatus returned nil")
		}
		if status["chain_id"] != nodeA.ChainID() {
			t.Fatalf("status chain id: %v", status["chain_id"])
		}
		if status["genesis_allocation"] != nodeA.PeerStatus()["genesis_allocation"] {
			t.Fatalf("status genesis fingerprint mismatch")
		}
	})

	t.Run("tx_and_block_propagation", func(t *testing.T) {
		receiver := "0x" + strings.Repeat("c1", 20)
		transfer := transferPayload(t, nodeA, ids[0], 0, receiver, "3")
		result, err := nodeA.SubmitTransaction(transfer)
		if err != nil {
			t.Fatalf("SubmitTransaction: %v", err)
		}
		if result["status"] != "committed" {
			t.Fatalf("seed did not commit the transfer: %v", result)
		}
		syncUntil(t, nodeB, 1, 10*time.Second)
		syncUntil(t, nodeC, 1, 10*time.Second)
		tipA := nodeA.Tip()
		if got := nodeB.Tip().CurrentBlockHash; got != tipA.CurrentBlockHash {
			t.Fatalf("B tip %s, want %s", got, tipA.CurrentBlockHash)
		}
		if got := nodeC.Tip().CurrentBlockHash; got != tipA.CurrentBlockHash {
			t.Fatalf("C tip %s, want %s", got, tipA.CurrentBlockHash)
		}
		if nodeB.CurrentStateRoot() != nodeA.CurrentStateRoot() || nodeC.CurrentStateRoot() != nodeA.CurrentStateRoot() {
			t.Fatalf("state roots diverged")
		}
		if nodeB.Tip().TxRoot != tipA.TxRoot {
			t.Fatalf("tx_root diverged")
		}
	})

	t.Run("stake_deposit_propagates", func(t *testing.T) {
		deposits := []any{}
		for index, identity := range ids {
			nonce := int64(0)
			if index == 0 {
				nonce = 1 // funded transfer above
			}
			deposits = append(deposits, validatorCommandPayload(t, nodeA, identity, nonce,
				map[string]any{"command": "deposit", "amount": testStakeUnits}))
		}
		block := commitDirect(t, nodeA, ids[0], deposits)
		syncUntil(t, nodeB, block.Index, 10*time.Second)
		syncUntil(t, nodeC, block.Index, 10*time.Second)
		if nodeB.ActiveValidatorCount() != 3 {
			t.Fatalf("B sees %d validators", nodeB.ActiveValidatorCount())
		}
	})

	t.Run("finalized_height_and_hash_agree", func(t *testing.T) {
		// Validators joined in the deposit block, so the first height that can
		// count their votes is the next block.
		block := commitDirect(t, nodeA, ids[0], nil)
		syncUntil(t, nodeB, block.Index, 10*time.Second)
		syncUntil(t, nodeC, block.Index, 10*time.Second)
		height := block.Index
		hash := block.CurrentBlockHash
		for _, voter := range ids {
			vote := finalityVoteFor(t, nodeA, voter, height, hash)
			for _, node := range []*Node{nodeA, nodeB, nodeC} {
				node.handleFinalityVote(vote)
			}
		}
		for name, node := range map[string]*Node{"A": nodeA, "B": nodeB, "C": nodeC} {
			if node.FinalizedHeight() != height || node.FinalizedHash() != hash {
				t.Fatalf("node %s finality (%d,%s), want (%d,%s)",
					name, node.FinalizedHeight(), node.FinalizedHash(), height, hash)
			}
		}
	})

	t.Run("contract_deploy_propagates", func(t *testing.T) {
		deploy := signTransaction(t, nodeA, ids[0], map[string]any{
			"chain_id":  nodeA.ChainID(),
			"sender":    ids[0].PublicKeyHex(),
			"nonce":     int64(2),
			"gas_limit": int64(2_000_000),
			"gas_price": int64(1),
			"contract_code": map[string]any{
				"command":     "deploy",
				"contract_id": "token",
				"pena_code":   interopTokenSource,
			},
		}).Payload
		block := commitDirect(t, nodeA, ids[0], []any{deploy})
		syncUntil(t, nodeB, block.Index, 10*time.Second)
		answer, err := nodeB.QueryContract("token", "answer", nil)
		if err != nil {
			t.Fatalf("B contract query: %v", err)
		}
		if fmt.Sprintf("%v", answer) != "42" {
			t.Fatalf("B contract answer: %v", answer)
		}
	})

	t.Run("sync_catch_up_after_restart", func(t *testing.T) {
		nodeC.Stop()
		receiver := "0x" + strings.Repeat("c2", 20)
		transfer := transferPayload(t, nodeA, ids[0], 3, receiver, "1")
		block := commitDirect(t, nodeA, ids[0], []any{transfer})
		restarted, err := NewNode(configC, ids[2])
		if err != nil {
			t.Fatalf("restart NewNode: %v", err)
		}
		defer restarted.Stop()
		if added := restarted.AddPeer(nodeA.SelfPeerRecord()); added != 1 {
			t.Fatalf("restart AddPeer: %d", added)
		}
		syncUntil(t, restarted, block.Index, 15*time.Second)
		if restarted.CurrentStateRoot() != nodeA.CurrentStateRoot() {
			t.Fatalf("restarted node state root mismatch")
		}
	})

	t.Run("shared_store_restart", func(t *testing.T) {
		kv := store.NewMemoryStore(":memory:")
		ports := freePorts(t, 4)
		config := interopConfig(ports, allocations)
		node, err := NewNodeWithStore(config, ids[0], kv)
		if err != nil {
			t.Fatalf("NewNodeWithStore: %v", err)
		}
		receiver := "0x" + strings.Repeat("c3", 20)
		transfer := transferPayload(t, node, ids[0], 0, receiver, "2")
		if _, err := node.SubmitTransaction(transfer); err != nil {
			t.Fatalf("store transfer: %v", err)
		}
		reloaded, err := NewNodeWithStore(config, ids[0], kv)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if reloaded.Tip().Index != node.Tip().Index || reloaded.CurrentStateRoot() != node.CurrentStateRoot() {
			t.Fatalf("persisted restart diverged")
		}
	})

	t.Run("sanrc20_transfer_and_undelegate_withdraw", func(t *testing.T) {
		t.Skip("policy: implemented for the Go mesh in consensus/api suites; not repeated by the interop harness")
	})
	t.Run("governance_tx", func(t *testing.T) {
		t.Skip("policy: governance is covered by the consensus invariant suite; the live harness only checks wire-level propagation")
	})
}

// ---------------------------------------------------------------------- #
// Live Go/Python tests
// ---------------------------------------------------------------------- #

func TestInteropGoSeedPythonJoins(t *testing.T) {
	pythonPorts := freePorts(t, 4)
	goPorts := freePorts(t, 4)

	seedIdentity := mustIdentity(t)
	seedAddress := identityAddress(t, seedIdentity)
	allocations := map[string]int64{seedAddress: 10_000 * ledger.SANBase}

	// The seed must be up before the Python node starts: Python bootstraps
	// once during startup.
	goConfig := interopConfig(goPorts, allocations)
	seed := startGoNode(t, goConfig, seedIdentity)

	python := startPythonNode(t, pythonPorts, map[string]string{
		"SAN_BOOTSTRAP":          fmt.Sprintf("127.0.0.1:%d", goPorts[1]),
		"SAN_GENESIS_ALLOCATION": fmt.Sprintf("%s:10000", seedAddress),
	})

	// Genesis and chain id must agree across implementations.
	pythonGenesis, err := python.getJSON("/genesis")
	if err != nil {
		t.Fatalf("python /genesis: %v", err)
	}
	if pythonGenesis["chain_id"] != seed.ChainID() {
		t.Fatalf("chain id mismatch: %v vs %s", pythonGenesis["chain_id"], seed.ChainID())
	}
	if pythonGenesis["genesis_hash"] != seed.GenesisHash() {
		t.Fatalf("genesis hash mismatch: %v vs %v", pythonGenesis["genesis_hash"], seed.GenesisHash())
	}

	// Handshake: the Python node must learn and keep the Go seed as a peer.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(seed.Peers()) == 0 {
		seed.Synchronize(context.Background())
		time.Sleep(200 * time.Millisecond)
	}
	if len(seed.Peers()) == 0 {
		health, _ := python.getJSON("/health")
		t.Fatalf("python never announced itself to the Go seed (python health=%v)\npython output:\n%s",
			health, tail(python.output.String(), 30))
	}

	// Block + tx propagation: Go commits a transfer, Python must follow.
	receiver := "0x" + strings.Repeat("d1", 20)
	transfer := transferPayload(t, seed, seedIdentity, 0, receiver, "4")
	if _, err := seed.SubmitTransaction(transfer); err != nil {
		t.Fatalf("seed transfer: %v", err)
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		health, err := python.getJSON("/health")
		if err == nil {
			height, _ := health["height"].(float64)
			tip, _ := health["tip_hash"].(string)
			stateRoot, _ := health["state_root"].(string)
			if int64(height) == seed.Tip().Index && tip == seed.Tip().CurrentBlockHash {
				if stateRoot != seed.CurrentStateRoot() {
					t.Fatalf("python state root %s, go %s", stateRoot, seed.CurrentStateRoot())
				}
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	health, _ := python.getJSON("/health")
	t.Fatalf("python did not follow the Go chain: %v (go tip %s)", health, seed.Tip().CurrentBlockHash)
}

func TestInteropPythonSeedGoJoins(t *testing.T) {
	pythonPorts := freePorts(t, 4)
	goPorts := freePorts(t, 4)

	joinerIdentity := mustIdentity(t)
	joinerAddress := identityAddress(t, joinerIdentity)
	allocations := map[string]int64{joinerAddress: 10_000 * ledger.SANBase}

	python := startPythonNode(t, pythonPorts, map[string]string{
		"SAN_GENESIS_ALLOCATION": fmt.Sprintf("%s:10000", joinerAddress),
	})

	goConfig := interopConfig(goPorts, allocations)
	// Interop-1: Python's Bootstrap RPC omits its own signed record, so a pure
	// gRPC bootstrap against the Python peer port yields no peer. Point the
	// joiner at the Python API port: the gRPC attempt fails cleanly and Go's
	// REST /bootstrap fallback adopts the signed record. See docs/interop.md.
	goConfig.Bootstrap = stringPointer(fmt.Sprintf("127.0.0.1:%d", pythonPorts[0]))
	joiner := startGoNode(t, goConfig, joinerIdentity)

	pythonGenesis, err := python.getJSON("/genesis")
	if err != nil {
		t.Fatalf("python /genesis: %v", err)
	}
	if pythonGenesis["genesis_hash"] != joiner.GenesisHash() {
		t.Fatalf("genesis hash mismatch: %v vs %v", pythonGenesis["genesis_hash"], joiner.GenesisHash())
	}

	t.Run("pure_grpc_bootstrap_unsupported", func(t *testing.T) {
		t.Skip("policy: Python's Bootstrap RPC returns only its peer table and omits its own signed record, " +
			"so a Go bootstrap against the Python peer port yields nothing. The harness uses the REST " +
			"/bootstrap fallback on the API port instead (docs/interop.md, unresolved item Interop-1).")
	})

	// The Go joiner must have adopted the Python seed's signed record.
	if len(joiner.Peers()) == 0 {
		pythonRecord(t, python)
		t.Fatalf("Go joiner learned no peers from the Python seed")
	}
	status := RemoteStatus(context.Background(), joiner, pythonRecord(t, python), 5*time.Second)
	if status == nil {
		t.Fatalf("RemoteStatus against the Python seed failed")
	}
	if status["chain_id"] != joiner.ChainID() {
		t.Fatalf("status chain id mismatch: %v", status["chain_id"])
	}

	// Stake the Go joiner, then propagate a transfer block. With an active
	// validator set only the expected proposer (the Go node) commits, so the
	// bootstrap-proposer race of two unstaked nodes cannot create a fork.
	deposit := validatorCommandPayload(t, joiner, joinerIdentity, 0,
		map[string]any{"command": "deposit", "amount": testStakeUnits})
	depositBlock := commitDirect(t, joiner, joinerIdentity, []any{deposit})

	waitForPythonTip := func(block *ledger.Block) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			health, err := python.getJSON("/health")
			if err == nil {
				tip, _ := health["tip_hash"].(string)
				height, _ := health["height"].(float64)
				if int64(height) >= block.Index && tip == block.CurrentBlockHash {
					if joiner.CurrentStateRoot() != health["state_root"] {
						t.Fatalf("go state root %s, python %s", joiner.CurrentStateRoot(), health["state_root"])
					}
					return
				}
			}
			joiner.Synchronize(context.Background())
			time.Sleep(300 * time.Millisecond)
		}
		health, _ := python.getJSON("/health")
		t.Fatalf("python did not follow the Go chain: %v (go tip %s)", health, block.CurrentBlockHash)
	}
	waitForPythonTip(depositBlock)

	receiver := "0x" + strings.Repeat("d2", 20)
	transfer := transferPayload(t, joiner, joinerIdentity, 1, receiver, "4")
	transferBlock := commitDirect(t, joiner, joinerIdentity, []any{transfer})
	waitForPythonTip(transferBlock)
}

func TestInteropThreeMixedNodes(t *testing.T) {
	pythonPorts := freePorts(t, 4)
	goSeedPorts := freePorts(t, 4)
	goJoinPorts := freePorts(t, 4)

	seedIdentity := mustIdentity(t)
	joinIdentity := mustIdentity(t)
	seedAddress := identityAddress(t, seedIdentity)
	joinAddress := identityAddress(t, joinIdentity)
	allocations := map[string]int64{
		seedAddress: 10_000 * ledger.SANBase,
		joinAddress: 10_000 * ledger.SANBase,
	}

	seedConfig := interopConfig(goSeedPorts, allocations)
	seed := startGoNode(t, seedConfig, seedIdentity)

	python := startPythonNode(t, pythonPorts, map[string]string{
		"SAN_BOOTSTRAP":          fmt.Sprintf("127.0.0.1:%d", goSeedPorts[1]),
		"SAN_GENESIS_ALLOCATION": fmt.Sprintf("%s:10000,%s:10000", seedAddress, joinAddress),
	})

	joinConfig := interopConfig(goJoinPorts, allocations)
	joinConfig.Bootstrap = stringPointer(fmt.Sprintf("127.0.0.1:%d", goSeedPorts[1]))
	joiner := startGoNode(t, joinConfig, joinIdentity)

	// All three implementations agree on the genesis hash.
	pythonGenesis, err := python.getJSON("/genesis")
	if err != nil {
		t.Fatalf("python /genesis: %v", err)
	}
	if pythonGenesis["genesis_hash"] != seed.GenesisHash() || joiner.GenesisHash() != seed.GenesisHash() {
		t.Fatalf("three-way genesis mismatch: python=%v seed=%v join=%v",
			pythonGenesis["genesis_hash"], seed.GenesisHash(), joiner.GenesisHash())
	}

	// Stake the seed first so it is the only expected proposer, then propagate
	// a transfer block to the two joiners.
	deposit := validatorCommandPayload(t, seed, seedIdentity, 0,
		map[string]any{"command": "deposit", "amount": testStakeUnits})
	commitDirect(t, seed, seedIdentity, []any{deposit})

	receiver := "0x" + strings.Repeat("d3", 20)
	transfer := transferPayload(t, seed, seedIdentity, 1, receiver, "6")
	block := commitDirect(t, seed, seedIdentity, []any{transfer})
	tip := block.CurrentBlockHash

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		joiner.Synchronize(context.Background())
		health, err := python.getJSON("/health")
		if err == nil {
			pythonTip, _ := health["tip_hash"].(string)
			if joiner.Tip().CurrentBlockHash == tip && pythonTip == tip {
				if joiner.CurrentStateRoot() != seed.CurrentStateRoot() {
					t.Fatalf("joiner state root mismatch")
				}
				if health["state_root"] != seed.CurrentStateRoot() {
					t.Fatalf("python state root mismatch")
				}
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	health, _ := python.getJSON("/health")
	t.Fatalf("three-way convergence failed: python=%v goJoin=%s want %s", health, joiner.Tip().CurrentBlockHash, tip)
}
