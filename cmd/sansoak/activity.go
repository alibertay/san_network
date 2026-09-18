package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alibertay/san_network/internal/bench"
	"github.com/alibertay/san_network/internal/devnet"
)

// kvContractSource is the simple storage contract used for write/read
// activity (the same source cmd/sane2e deploys).
const kvContractSource = `data := {}
counter := 0

function set(k, v) {
  data[k] = v
}
function get(k) {
  return data[k]
}
function inc() {
  counter = counter + 1
  return counter
}
function count() {
  return counter
}
`

// readContractSource loads a PENA example from the repository.
func readContractSource(directory, name string) (string, error) {
	root, err := bench.RepoRoot()
	if err != nil {
		return "", err
	}
	if directory == "" {
		return kvContractSource, nil
	}
	data, err := os.ReadFile(filepath.Join(root, "PENA", "examples", directory, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// tick runs one activity step. Activity is sequential by design: it keeps
// the RNG deterministic, avoids nonce races and lets sampling observe a
// quiescent-enough chain.
func (s *soakState) tick(cycles int64) {
	if s.nodes[0].spec.URL != "" {
		// RPC mode: read-only probing only.
		s.actionRead()
		return
	}
	actions := []func(){
		s.actionTransfer,
		s.actionSANRC20,
		s.actionKVWrite,
		s.actionStake,
		s.actionRead,
	}
	if !s.runner.short {
		actions = append(actions, s.actionGovernance, s.actionChurn)
	}
	index := int(cycles-1) % len(actions)
	if cycles > 2 {
		index = s.randomInt(len(actions))
	}
	actions[index]()
}

func (s *soakState) record(err error, detail string) {
	if err != nil {
		s.activityErrors++
		s.runner.logf("activity %s failed: %v", detail, err)
		return
	}
	s.activityOK++
}

// actionTransfer moves 1 SAN between two node wallets.
func (s *soakState) actionTransfer() {
	if len(s.nodes) < 2 {
		return
	}
	from := s.nodes[s.randomInt(len(s.nodes))]
	to := s.nodes[s.randomInt(len(s.nodes))]
	if from == to {
		to = s.nodes[(s.randomInt(len(s.nodes)))%len(s.nodes)]
	}
	if from.wallet == "" || to.wallet == "" {
		return
	}
	_, err := s.send(from, map[string]any{"receiver": to.wallet, "value": "1"})
	s.record(err, "transfer")
}

// actionSANRC20 transfers SANRC20 tokens between node wallets.
func (s *soakState) actionSANRC20() {
	if !s.deployed["sanrc20"] || len(s.nodes) < 2 {
		return
	}
	source := s.nodes[0]
	target := s.nodes[1+s.randomInt(len(s.nodes)-1)]
	if target.wallet == "" {
		return
	}
	_, err := s.send(source, contractCallFields("sanrc20", "transfer",
		[]any{source.wallet, target.wallet, int64(1)}))
	if err == nil {
		_, err = s.harness.Client(source.spec, false).ContractQuery("sanrc20", "balanceOf", []any{target.wallet})
	}
	s.record(err, "sanrc20 transfer")
}

// actionKVWrite writes and reads the key/value contract.
func (s *soakState) actionKVWrite() {
	if !s.deployed["kv"] {
		return
	}
	node := s.nodes[s.randomInt(len(s.nodes))]
	if node.wallet == "" {
		return
	}
	key := fmt.Sprintf("k%d", s.randomInt(1000))
	_, err := s.send(node, contractCallFields("kv", "set", []any{key, int64(s.randomInt(100000))}))
	if err == nil {
		_, err = s.harness.Client(node.spec, false).ContractQuery("kv", "get", []any{key})
	}
	s.record(err, "kv set/get")
}

// actionStake advances the staking lifecycle on a joiner wallet: deposit,
// then (long mode only) undelegate after a delay and withdraw once the
// unbonding period elapsed.
func (s *soakState) actionStake() {
	if len(s.nodes) < 2 {
		return
	}
	node := s.nodes[1+s.randomInt(len(s.nodes)-1)]
	if node.wallet == "" {
		return
	}
	switch node.stakePhase {
	case 0:
		account, err := s.harness.Client(node.spec, false).Account(node.wallet)
		if err != nil || devnet.ToInt64(account["balance_units"]) < 150*int64(1e8) {
			return
		}
		if _, err := s.depositStake(node, 100); err != nil {
			s.record(err, "stake deposit")
			return
		}
		node.stakePhase = 1
		s.activityOK++
	case 1:
		if s.runner.short {
			return
		}
		if _, err := s.send(node, map[string]any{"validator": map[string]any{"command": "undelegate"}}); err != nil {
			s.record(err, "undelegate")
			return
		}
		node.stakePhase = 2
		node.undelegatedAt = time.Now()
		s.activityOK++
	case 2:
		if s.runner.short {
			return
		}
		// The unbonding period is measured in blocks; the devnet produces
		// roughly one block per second, so wait that many seconds plus slack.
		unbonding := 110.0
		if s.unbondingSeconds > 0 {
			unbonding = s.unbondingSeconds + 10
		}
		if time.Since(node.undelegatedAt).Seconds() < unbonding {
			return
		}
		if _, err := s.send(node, map[string]any{"validator": map[string]any{"command": "withdraw"}}); err != nil {
			s.record(err, "withdraw")
			return
		}
		node.stakePhase = 0
		s.activityOK++
	}
}

// actionGovernance submits one safe no-op parameter change (same value as the
// genesis parameter) approved by the staked validator.
func (s *soakState) actionGovernance() {
	if s.governanceOK {
		return
	}
	validator := s.stakedNode()
	if validator == nil {
		return
	}
	name := "min_block_interval_ms"
	value := s.genesisParameter(name)
	if value == 0 {
		return
	}
	client := s.harness.Client(validator.spec, true)
	approval, err := client.GovernanceApproval(name, value, &validator.localNonce)
	if err != nil {
		s.record(err, "governance approval")
		return
	}
	_, err = s.send(validator, map[string]any{"governance": map[string]any{
		"command":   "set_param",
		"name":      name,
		"value":     value,
		"approvals": []any{approval},
	}})
	if err != nil {
		s.record(err, "governance set_param")
		return
	}
	s.governanceOK = true
	s.activityOK++
}

// actionChurn restarts a random joiner after a cooldown, which exercises
// catch-up, reconnection and the readiness transitions.
func (s *soakState) actionChurn() {
	if len(s.nodes) < 2 {
		return
	}
	if !s.lastRestart.IsZero() && time.Since(s.lastRestart) < 45*time.Second {
		return
	}
	node := s.nodes[1+s.randomInt(len(s.nodes)-1)]
	s.lastRestart = time.Now()
	s.runner.logf("churn: restarting %s", node.name)
	if err := s.harness.StopNode(node.spec); err != nil {
		s.runner.logf("churn: stop %s: %v", node.name, err)
	}
	time.Sleep(2 * time.Second)
	if err := s.harness.StartJoiner(node.spec); err != nil {
		s.record(err, "churn restart")
		return
	}
	if err := s.harness.WaitHealthy(node.spec, 60*time.Second); err != nil {
		s.record(err, "churn healthy")
		return
	}
	if err := s.harness.WaitSynced(node.spec, 60*time.Second); err != nil {
		s.record(err, "churn sync")
		return
	}
	s.activityOK++
}

// actionRead queries state without submitting anything.
func (s *soakState) actionRead() {
	node := s.nodes[s.randomInt(len(s.nodes))]
	client := s.harness.Client(node.spec, false)
	if s.deployed["kv"] {
		if _, err := client.ContractQuery("kv", "count", nil); err != nil {
			s.record(err, "contract read")
			return
		}
	}
	if node.wallet != "" {
		if _, err := client.Account(node.wallet); err != nil {
			s.record(err, "account read")
			return
		}
	}
	s.activityOK++
}

// stakedNode returns a validator wallet that can approve governance changes.
func (s *soakState) stakedNode() *runnerNode {
	for _, node := range s.nodes {
		if node.stakePhase == 1 {
			return node
		}
	}
	return nil
}

// genesisParameter returns a genesis parameter in base units.
func (s *soakState) genesisParameter(name string) int64 {
	genesis, err := s.harness.Client(s.nodes[0].spec, false).Genesis()
	if err != nil {
		return 0
	}
	parameters, _ := genesis["parameters"].(map[string]any)
	return parseIntValue(parameters[name])
}

// unbondingSeconds caches the unbonding period (in blocks, ~= seconds).
func (s *soakState) setupUnbonding() {
	validators, err := s.harness.Client(s.nodes[0].spec, false).Validators()
	if err != nil {
		return
	}
	period := devnet.ToInt64(validators["unbonding_period"])
	if period > 0 {
		s.unbondingSeconds = float64(period)
	}
}
