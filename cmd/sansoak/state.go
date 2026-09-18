package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/devnet"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/soak"
)

// runnerNode is one node under soak.
type runnerNode struct {
	name       string
	spec       *devnet.NodeSpec
	wallet     string
	localNonce int64
	// Stake phase: 0 = not staked, 1 = staked, 2 = undelegated.
	stakePhase    int
	undelegatedAt time.Time
}

// soakState is the sequential activity/sampling state.
type soakState struct {
	runner  *runner
	harness *devnet.Harness
	nodes   []*runnerNode

	lastView  map[string]soak.NodeView
	firstView map[string]soak.NodeView

	cycles         int64
	transactions   int64
	activityOK     int64
	activityErrors int64

	genesisTotal     int64
	blockReward      int64
	unbondingSeconds float64
	moneyCheck       bool

	deployed     map[string]bool
	governanceOK bool
	lastRestart  time.Time

	activityIndex int
	deterministic *big.Int
}

func newSoakState(harness *devnet.Harness, runner *runner) (*soakState, error) {
	state := &soakState{
		runner:        runner,
		harness:       harness,
		lastView:      map[string]soak.NodeView{},
		firstView:     map[string]soak.NodeView{},
		deployed:      map[string]bool{},
		deterministic: big.NewInt(runner.seed),
	}
	for _, spec := range harness.Nodes {
		node := &runnerNode{name: spec.Name, spec: spec, wallet: spec.Address}
		state.nodes = append(state.nodes, node)
	}
	if len(state.nodes) == 0 {
		return nil, fmt.Errorf("no nodes configured")
	}
	return state, nil
}

func (s *soakState) close() {}

// setup starts (or verifies) the nodes and prepares funded wallets and
// contracts. In RPC mode it only verifies the targets are healthy.
func (s *soakState) setup() error {
	rpcMode := s.nodes[0].spec.URL != ""
	if !rpcMode {
		if err := s.startSpawnDevnet(); err != nil {
			return err
		}
	} else {
		for _, node := range s.nodes {
			if err := s.harness.WaitHealthy(node.spec, 30*time.Second); err != nil {
				return err
			}
		}
	}

	if err := s.readGenesis(); err != nil {
		return err
	}
	if !rpcMode {
		if err := s.fundAndDeploy(); err != nil {
			return err
		}
		s.setupUnbonding()
	}
	s.sample(false)
	return nil
}

// startSpawnDevnet starts the founder seed, stakes it (so the expected
// proposer is deterministic before joiners arrive) and only then starts the
// joiners. This prevents the no-validator fork window that makes a soak flaky
// on slow hosts.
func (s *soakState) startSpawnDevnet() error {
	seed := s.nodes[0].spec
	s.runner.logf("starting founder %s with faucet and 0 stake", seed.Name)
	if err := s.harness.StartSeed(seed); err != nil {
		return err
	}
	if err := s.harness.WaitHealthy(seed, 90*time.Second); err != nil {
		return err
	}
	if err := s.harness.WaitHeight(seed, 2, 60*time.Second); err != nil {
		return err
	}
	if err := s.readGenesis(); err != nil {
		return err
	}
	s.runner.logf("staking the founder with 100 SAN")
	if _, err := s.depositStake(s.nodes[0], 100); err != nil {
		return fmt.Errorf("founder stake: %w", err)
	}
	s.nodes[0].stakePhase = 1
	if err := s.waitActive(s.nodes[0], 60*time.Second); err != nil {
		return fmt.Errorf("founder validator activation: %w", err)
	}

	for _, node := range s.nodes[1:] {
		s.runner.logf("starting joiner %s", node.name)
		if err := s.harness.StartJoiner(node.spec); err != nil {
			return err
		}
	}
	for _, node := range s.nodes[1:] {
		if err := s.harness.WaitHealthy(node.spec, 90*time.Second); err != nil {
			return err
		}
		if err := s.harness.WaitPeers(node.spec, 1, 60*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// readGenesis caches the immutable supply inputs used by the money
// conservation invariant.
func (s *soakState) readGenesis() error {
	client := s.harness.Client(s.nodes[0].spec, false)
	genesis, err := client.Genesis()
	if err != nil {
		return fmt.Errorf("cannot read /genesis: %w", err)
	}
	allocations, _ := genesis["genesis_allocation"].(map[string]any)
	total := int64(0)
	for _, raw := range allocations {
		total += parseIntValue(raw)
	}
	s.genesisTotal = total
	if parameters, ok := genesis["parameters"].(map[string]any); ok {
		s.blockReward = parseIntValue(parameters["block_reward"])
	}
	s.moneyCheck = total > 0 && s.genesisTotal > 0
	for _, node := range s.nodes {
		if node.wallet == "" {
			s.moneyCheck = false
		}
	}
	return nil
}

// fundAndDeploy transfers starter funds to the joiners, stakes one more
// validator and deploys the activity contracts. Every step waits for the
// chain to confirm it so the activity phase starts from a settled state.
func (s *soakState) fundAndDeploy() error {
	if len(s.nodes) < 2 {
		return nil
	}
	seed := s.nodes[0]
	for _, node := range s.nodes[1:] {
		if node.wallet == "" {
			continue
		}
		if _, err := s.send(seed, map[string]any{"receiver": node.wallet, "value": "500"}); err != nil {
			return fmt.Errorf("funding %s: %w", node.name, err)
		}
		if err := s.waitBalance(seed, node.wallet, 500, 60*time.Second); err != nil {
			return fmt.Errorf("funding %s: %w", node.name, err)
		}
	}
	// Stake a second validator so validator-set changes and finality votes
	// exercise more than one wallet.
	stakeTarget := s.nodes[1]
	if _, err := s.depositStake(stakeTarget, 100); err != nil {
		return fmt.Errorf("stake %s: %w", stakeTarget.name, err)
	}
	stakeTarget.stakePhase = 1
	if err := s.waitActive(stakeTarget, 60*time.Second); err != nil {
		return fmt.Errorf("validator activation %s: %w", stakeTarget.name, err)
	}
	if err := s.deployContracts(); err != nil {
		// Deploy failures are activity errors, not invariants; keep soaking.
		s.activityErrors++
		s.runner.logf("warning: contract deployment failed: %v", err)
	}
	return nil
}

// waitActive polls /validators until the node's wallet is an active
// validator.
func (s *soakState) waitActive(node *runnerNode, timeout time.Duration) error {
	if node.wallet == "" {
		return nil
	}
	client := s.harness.Client(s.nodes[0].spec, false)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		validators, err := client.Validators()
		if err == nil {
			records, _ := validators["validators"].([]any)
			for _, raw := range records {
				if record, ok := raw.(map[string]any); ok {
					if address, _ := record["address"].(string); address == node.wallet {
						return nil
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("wallet %s never became an active validator", node.wallet)
}

// waitBalance polls /account until the address holds at least the units.
func (s *soakState) waitBalance(node *runnerNode, address string, san int64, timeout time.Duration) error {
	units, err := ledger.SanToUnits(san)
	if err != nil {
		return err
	}
	client := s.harness.Client(node.spec, false)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		account, err := client.Account(address)
		if err == nil && devnet.ToInt64(account["balance_units"]) >= units {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("balance of %s did not reach %d SAN", address, san)
}

// deployContracts deploys the SANRC20 and key/value contracts from the seed
// and waits until they are visible.
func (s *soakState) deployContracts() error {
	if s.nodes[0].wallet == "" {
		return nil
	}
	seed := s.nodes[0]
	sanrc20, err := readContractSource("SANRC20", "SANRC20.pena")
	if err != nil {
		return err
	}
	if _, err := s.send(seed, contractDeployFields("sanrc20", sanrc20)); err != nil {
		return fmt.Errorf("sanrc20 deploy: %w", err)
	}
	if err := s.waitContract("sanrc20", 60*time.Second); err != nil {
		return fmt.Errorf("sanrc20 deploy: %w", err)
	}
	s.deployed["sanrc20"] = true
	if _, err := s.send(seed, contractCallFields("sanrc20", "init",
		[]any{"SAN Token", "SANRC20", int64(18), int64(1_000_000), seed.wallet})); err != nil {
		return fmt.Errorf("sanrc20 init: %w", err)
	}
	if _, err := s.send(seed, contractDeployFields("kv", kvContractSource)); err != nil {
		return fmt.Errorf("kv deploy: %w", err)
	}
	if err := s.waitContract("kv", 60*time.Second); err != nil {
		return fmt.Errorf("kv deploy: %w", err)
	}
	s.deployed["kv"] = true
	return nil
}

// waitContract polls /contracts until the contract id is deployed.
func (s *soakState) waitContract(contractID string, timeout time.Duration) error {
	client := s.harness.Client(s.nodes[0].spec, false)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		contracts, err := client.Contracts()
		if err == nil {
			for _, candidate := range contracts {
				if candidate == contractID {
					return nil
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("contract %s was not deployed", contractID)
}

// contractDeployFields builds a contract deploy payload.
func contractDeployFields(contractID, source string) map[string]any {
	return map[string]any{
		"gas_limit": int64(2_000_000),
		"gas_price": int64(1),
		"contract_code": map[string]any{
			"command":     "deploy",
			"contract_id": contractID,
			"pena_code":   source,
		},
	}
}

// contractCallFields builds a contract run payload.
func contractCallFields(contractID, function string, params []any) map[string]any {
	if params == nil {
		params = []any{}
	}
	return map[string]any{
		"gas_limit": int64(1_000_000),
		"gas_price": int64(1),
		"contract_code": map[string]any{
			"command":       "run",
			"contract_id":   contractID,
			"function_name": function,
			"params":        params,
		},
	}
}

// depositStake submits a validator deposit through the wallet's own nonce
// tracker.
func (s *soakState) depositStake(node *runnerNode, amountSAN int64) (map[string]any, error) {
	units, err := ledger.SanToUnits(amountSAN)
	if err != nil {
		return nil, err
	}
	return s.send(node, map[string]any{"validator": map[string]any{
		"command": "deposit",
		"amount":  units,
	}})
}

// send submits a signed payload using a locally tracked nonce so repeated
// activity from the same wallet never races with the mempool.
func (s *soakState) send(node *runnerNode, fields map[string]any) (map[string]any, error) {
	client := s.harness.Client(node.spec, true)
	deadline := time.Now().Add(30 * time.Second)
	for {
		nonce, err := client.Nonce("")
		if err == nil && nonce == node.localNonce {
			break
		}
		if err == nil && nonce > node.localNonce {
			// The node is ahead (e.g. after a restart where another actor
			// used this wallet); resynchronise instead of failing.
			node.localNonce = nonce
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("nonce wait timed out (local %d)", node.localNonce)
		}
		time.Sleep(250 * time.Millisecond)
	}
	nonce := node.localNonce
	result, err := client.Send(&nonce, fields)
	if err != nil {
		return nil, err
	}
	node.localNonce++
	s.transactions++
	return result, nil
}

// ---------------------------------------------------------------------- #
// Sampling
// ---------------------------------------------------------------------- #

// sample collects one NodeView per node. final marks the end-of-run sample.
func (s *soakState) sample(final bool) {
	for _, node := range s.nodes {
		view := s.sampleNode(node)
		view.StartHeight = s.startHeight(node.name, view.Height)
		if _, present := s.firstView[node.name]; !present {
			s.firstView[node.name] = view
		}
		s.lastView[node.name] = view
	}
}

func (s *soakState) startHeight(name string, fallback int64) int64 {
	if view, present := s.firstView[name]; present {
		return view.Height
	}
	return fallback
}

func (s *soakState) sampleNode(node *runnerNode) soak.NodeView {
	view := soak.NodeView{Name: node.name, MoneyCheck: s.moneyCheck}
	client := s.harness.Client(node.spec, false)
	health, err := client.Health()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	view.Height = devnet.ToInt64(health["height"])
	view.FinalizedHeight = devnet.ToInt64(health["finalized_height"])
	view.Mempool = devnet.ToInt64(health["mempool"])
	if root, ok := health["state_root"].(string); ok {
		view.StateRoot = root
	}
	if hash, ok := health["tip_hash"].(string); ok {
		view.TipHash = hash
	}
	if validators, err := client.Validators(); err == nil {
		records, _ := validators["validators"].([]any)
		for _, raw := range records {
			if record, ok := raw.(map[string]any); ok {
				if address, ok := record["address"].(string); ok {
					view.Validators = append(view.Validators, address)
				}
			}
		}
		view.TotalStake = devnet.ToInt64(validators["total_stake_units"])
	}
	for _, address := range s.knownAddresses() {
		account, err := client.Account(address)
		if err == nil {
			view.KnownBalance += devnet.ToInt64(account["balance_units"])
		}
		record, err := client.Stake(address)
		if err == nil {
			view.KnownStake += devnet.ToInt64(record["stake_units"])
		}
	}
	if text, err := client.MetricsText(); err == nil {
		metrics := parseMetrics(text)
		view.TotalBurned = int64(metrics["san_total_burned"])
		view.TotalSlashed = int64(metrics["san_total_slashed"])
		view.Goroutines = int64(metrics["san_go_goroutines"])
		view.MemoryAlloc = int64(metrics["san_go_memory_alloc_bytes"])
	}
	view.GenesisTotal = s.genesisTotal
	view.BlockReward = s.blockReward
	return view
}

func (s *soakState) knownAddresses() []string {
	addresses := []string{}
	for _, node := range s.nodes {
		if node.wallet != "" {
			addresses = append(addresses, node.wallet)
		}
	}
	return addresses
}

func (s *soakState) finalViews() []soak.NodeView {
	views := make([]soak.NodeView, 0, len(s.nodes))
	for _, node := range s.nodes {
		view, present := s.lastView[node.name]
		if !present {
			view = soak.NodeView{Name: node.name, Error: "no sample collected"}
		}
		views = append(views, view)
	}
	return views
}

// growthPoints compares the first and last sample's maximum goroutines and
// memory across nodes.
func (s *soakState) growthPoints() []soak.GrowthPoint {
	startGoroutines, endGoroutines := 0.0, 0.0
	startMemory, endMemory := 0.0, 0.0
	for name, view := range s.firstView {
		if float64(view.Goroutines) > startGoroutines {
			startGoroutines = float64(view.Goroutines)
		}
		if float64(view.MemoryAlloc) > startMemory {
			startMemory = float64(view.MemoryAlloc)
		}
		last := s.lastView[name]
		if float64(last.Goroutines) > endGoroutines {
			endGoroutines = float64(last.Goroutines)
		}
		if float64(last.MemoryAlloc) > endMemory {
			endMemory = float64(last.MemoryAlloc)
		}
	}
	return []soak.GrowthPoint{
		{Name: "goroutines", Start: startGoroutines, End: endGoroutines, MaxRatio: 4.0},
		{Name: "memory_alloc", Start: startMemory, End: endMemory, MaxRatio: 6.0},
	}
}

// activityCheck reports the submitted/errored activity ratio. A small number
// of rejected activities is expected (nonce races, transient sync); a high
// error rate is a report failure.
func (s *soakState) activityCheck() soak.CheckResult {
	total := s.activityOK + s.activityErrors
	if total == 0 {
		return soak.CheckResult{Name: "activity_submitted", Passed: true, Detail: "read-only run"}
	}
	rate := float64(s.activityOK) / float64(total)
	if rate < 0.5 {
		return soak.CheckResult{
			Name:   "activity_submitted",
			Passed: false,
			Detail: fmt.Sprintf("%d/%d activities failed", s.activityErrors, total),
		}
	}
	return soak.CheckResult{
		Name:   "activity_submitted",
		Passed: true,
		Detail: fmt.Sprintf("%d/%d activities accepted", s.activityOK, total),
	}
}

// parseMetrics parses the Prometheus text exposition into a name->value map.
func parseMetrics(text string) map[string]float64 {
	metrics := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := parseFloat(fields[1])
		if err != nil {
			continue
		}
		metrics[fields[0]] = value
	}
	return metrics
}

func parseFloat(raw string) (float64, error) {
	var value float64
	_, err := fmt.Sscanf(raw, "%g", &value)
	return value, err
}

// randomInt returns a deterministic pseudo-random integer in [0, max).
func (s *soakState) randomInt(max int) int {
	if max <= 0 {
		return 0
	}
	value := new(big.Int).Set(s.deterministic)
	value.Mul(value, big.NewInt(1103515245))
	value.Add(value, big.NewInt(12345))
	value.Mod(value, big.NewInt(int64(max)))
	s.deterministic = new(big.Int).Set(value)
	return int(value.Int64())
}

// randomDuration is used for jittered activity selection.
func (s *soakState) cryptoRandom() float64 {
	max := big.NewInt(1 << 31)
	value, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0
	}
	return float64(value.Int64()) / float64(int64(1)<<31)
}
