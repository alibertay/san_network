package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

func (h *harness) stepStartDevnet(stdout io.Writer) error {
	a := h.node("A-seed")
	b := h.node("B")
	c := h.node("C")

	logf(stdout, "-- starting the founder seed A with --stake 0 --")
	if err := h.runNode(a, "--seed", "--stake", "0", "--faucet"); err != nil {
		return err
	}
	if err := h.waitHealthy(a, 90*time.Second); err != nil {
		return err
	}
	if err := h.waitHeight(a, 1, 60*time.Second); err != nil {
		return err
	}
	if err := waitUntil(60*time.Second, "seed A to earn a block reward at 0 stake", func() bool {
		return h.balanceUnits(a, a.address) > 10000*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "0-stake block reward proof: A balance is now %s SAN",
		ledger.UnitsToSAN(h.balanceUnits(a, a.address)))

	logf(stdout, "-- reconciling A to --stake 100 while the node keeps running --")
	if err := h.runNode(a, "--stake", "100"); err != nil {
		return err
	}
	if err := waitUntil(120*time.Second, "A stake 100", func() bool {
		return h.stakeUnits(a, a.address) == 100*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "A is an active validator with 100 SAN staked")

	logf(stdout, "-- starting joiners B and C with NO --bootstrap argument --")
	if err := h.runNode(b); err != nil {
		return err
	}
	if err := h.runNode(c); err != nil {
		return err
	}
	for _, spec := range []*nodeSpec{b, c} {
		if err := h.waitHealthy(spec, 90*time.Second); err != nil {
			return err
		}
		if err := h.waitPeers(spec, 1, 60*time.Second); err != nil {
			return err
		}
		if err := h.waitHeight(spec, 1, 90*time.Second); err != nil {
			return err
		}
	}
	healthA, _ := h.client("A-seed", false).Health()
	healthB, _ := h.client("B", false).Health()
	healthC, _ := h.client("C", false).Health()
	logf(stdout, "discovery evidence: A peers=%v height=%v | B peers=%v height=%v | C peers=%v height=%v",
		healthA["peers"], healthA["height"], healthB["peers"], healthB["height"], healthC["peers"], healthC["height"])
	return nil
}

func (h *harness) stepTransfers(stdout io.Writer) error {
	b := h.node("B")
	c := h.node("C")

	if err := h.waitSynced(b, 60*time.Second); err != nil {
		return err
	}
	beforeB := h.balanceUnits(h.node("A-seed"), b.address)
	logf(stdout, "A -> B 4000 SAN; B before = %s SAN", ledger.UnitsToSAN(beforeB))
	result, err := h.client("A-seed", true).Transfer(b.address, "4000", nil)
	if err != nil {
		return fmt.Errorf("A -> B transfer failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "B balance +4000", func() bool {
		return h.balanceUnits(h.node("A-seed"), b.address) == beforeB+4000*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "tx %v status=%v; B after = %s SAN", result["tx_id"], result["status"],
		ledger.UnitsToSAN(h.balanceUnits(h.node("A-seed"), b.address)))

	if err := h.waitSynced(b, 60*time.Second); err != nil {
		return err
	}
	beforeC := h.balanceUnits(h.node("A-seed"), c.address)
	logf(stdout, "B -> C 2000 SAN; C before = %s SAN", ledger.UnitsToSAN(beforeC))
	result, err = h.client("B", true).Transfer(c.address, "2000", nil)
	if err != nil {
		return fmt.Errorf("B -> C transfer failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "C balance +2000", func() bool {
		return h.balanceUnits(h.node("A-seed"), c.address) == beforeC+2000*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "tx %v status=%v; C after = %s SAN", result["tx_id"], result["status"],
		ledger.UnitsToSAN(h.balanceUnits(h.node("A-seed"), c.address)))
	return nil
}

func (h *harness) stepSANRC20(stdout io.Writer) error {
	a := h.node("A-seed")
	b := h.node("B")
	c := h.node("C")
	source, err := os.ReadFile(filepath.Join(h.root, "PENA", "examples", "SANRC20", "SANRC20.pena"))
	if err != nil {
		return err
	}
	client := h.client("A-seed", true)
	result, err := client.DeployContract("sanrc20", string(source), sdk.DefaultDeployGasLimit, nil, nil)
	if err != nil {
		return fmt.Errorf("sanrc20 deploy failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "sanrc20 deploy", func() bool {
		contracts, err := h.client("A-seed", false).Contracts()
		if err != nil {
			return false
		}
		for _, contract := range contracts {
			if contract == "sanrc20" {
				return true
			}
		}
		return false
	}); err != nil {
		return err
	}
	logf(stdout, "deploy tx %v status=%v", result["tx_id"], result["status"])

	if _, err := client.CallContract("sanrc20", "init",
		[]any{"SAN Token", "SANRC20", int64(18), int64(1_000_000), a.address},
		sdk.DefaultCallGasLimit, nil, nil); err != nil {
		return fmt.Errorf("sanrc20 init failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "sanrc20 init", func() bool {
		value, err := h.query("sanrc20", "name", nil)
		return err == nil && value == "SAN Token"
	}); err != nil {
		return err
	}
	logf(stdout, "init: name=%v symbol=%v totalSupply=%v balanceOf(A)=%v",
		h.queryOr("sanrc20", "name", nil), h.queryOr("sanrc20", "symbol", nil),
		h.queryOr("sanrc20", "totalSupply", nil), h.queryOr("sanrc20", "balanceOf", []any{a.address}))

	if _, err := client.CallContract("sanrc20", "transfer",
		[]any{a.address, b.address, int64(150)}, sdk.DefaultCallGasLimit, nil, nil); err != nil {
		return fmt.Errorf("sanrc20 transfer failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "sanrc20 transfer", func() bool {
		value, err := h.query("sanrc20", "balanceOf", []any{b.address})
		return err == nil && asInt64(value) == 150
	}); err != nil {
		return err
	}
	if balance, _ := h.query("sanrc20", "balanceOf", []any{a.address}); asInt64(balance) != 999_850 {
		return fmt.Errorf("expected balanceOf(A)=999850, got %v", balance)
	}
	logf(stdout, "transfer 150: balanceOf(A)=%v balanceOf(B)=%v",
		h.queryOr("sanrc20", "balanceOf", []any{a.address}), h.queryOr("sanrc20", "balanceOf", []any{b.address}))

	if _, err := client.CallContract("sanrc20", "mint",
		[]any{a.address, c.address, int64(500)}, sdk.DefaultCallGasLimit, nil, nil); err != nil {
		return fmt.Errorf("sanrc20 mint failed: %w", err)
	}
	if err := waitUntil(60*time.Second, "sanrc20 mint", func() bool {
		value, err := h.query("sanrc20", "balanceOf", []any{c.address})
		return err == nil && asInt64(value) == 500
	}); err != nil {
		return err
	}
	if supply, _ := h.query("sanrc20", "totalSupply", nil); asInt64(supply) != 1_000_500 {
		return fmt.Errorf("expected totalSupply=1000500, got %v", supply)
	}
	logf(stdout, "mint 500: totalSupply=%v balanceOf(C)=%v",
		h.queryOr("sanrc20", "totalSupply", nil), h.queryOr("sanrc20", "balanceOf", []any{c.address}))
	return nil
}

const customContractSource = `data := {}
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

func (h *harness) stepCustomContract(stdout io.Writer) error {
	c := h.node("C")
	sourcePath := filepath.Join(h.temp, "kv.pena")
	if err := os.WriteFile(sourcePath, []byte(customContractSource), 0o644); err != nil {
		return err
	}
	if err := h.waitSynced(c, 60*time.Second); err != nil {
		return err
	}
	client := h.client("C", true)
	result, err := client.DeployContract("kv", customContractSource, sdk.DefaultDeployGasLimit, nil, nil)
	if err != nil {
		return fmt.Errorf("kv deploy failed: %w", err)
	}
	for _, spec := range []*nodeSpec{h.node("A-seed"), c} {
		if err := waitUntil(60*time.Second, fmt.Sprintf("kv deploy on %s", spec.name), func() bool {
			contracts, err := h.client(spec.name, false).Contracts()
			if err != nil {
				return false
			}
			for _, contract := range contracts {
				if contract == "kv" {
					return true
				}
			}
			return false
		}); err != nil {
			return err
		}
	}
	logf(stdout, "deploy tx %v status=%v", result["tx_id"], result["status"])

	expectedNonce := int64(1)
	call := func(function string, params []any) error {
		if err := h.waitSynced(c, 60*time.Second); err != nil {
			return err
		}
		if err := waitUntil(60*time.Second, "C nonce to catch up", func() bool {
			return h.nonce(c, c.address) >= expectedNonce
		}); err != nil {
			return err
		}
		if _, err := client.CallContract("kv", function, params, sdk.DefaultCallGasLimit, nil, nil); err != nil {
			return fmt.Errorf("kv %s failed: %w", function, err)
		}
		expectedNonce++
		return nil
	}

	if err := call("set", []any{"alpha", int64(42)}); err != nil {
		return err
	}
	if err := waitUntil(60*time.Second, "kv set/get", func() bool {
		value, err := h.query("kv", "get", []any{"alpha"})
		return err == nil && asInt64(value) == 42
	}); err != nil {
		return err
	}
	if err := call("inc", nil); err != nil {
		return err
	}
	if err := call("inc", nil); err != nil {
		return err
	}
	if err := waitUntil(60*time.Second, "kv count=2", func() bool {
		value, err := h.query("kv", "count", nil)
		return err == nil && asInt64(value) == 2
	}); err != nil {
		return err
	}
	logf(stdout, "custom contract kv: set('alpha',42) -> get('alpha')=%v; inc()x2 -> count()=%v",
		h.queryOr("kv", "get", []any{"alpha"}), h.queryOr("kv", "count", nil))
	return nil
}

func (h *harness) stepStake(stdout io.Writer) error {
	b := h.node("B")
	logf(stdout, "on-chain stake for B (%s) before: %s SAN",
		b.address, ledger.UnitsToSAN(h.stakeUnits(h.node("A-seed"), b.address)))

	logf(stdout, "-- run 1: B with --stake 100 --")
	if err := h.runNode(b, "--stake", "100"); err != nil {
		return err
	}
	if err := waitUntil(120*time.Second, "B stake 100", func() bool {
		return h.stakeUnits(h.node("A-seed"), b.address) == 100*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "on-chain stake after 100: %s SAN", ledger.UnitsToSAN(h.stakeUnits(h.node("A-seed"), b.address)))

	logf(stdout, "-- run 2: B with --stake 70 (undelegate-all + withdraw + re-stake) --")
	if err := h.runNode(b, "--stake", "70"); err != nil {
		return err
	}
	if err := waitUntil(120*time.Second, "B stake 70", func() bool {
		return h.stakeUnits(h.node("A-seed"), b.address) == 70*sanBase
	}); err != nil {
		return err
	}
	logf(stdout, "on-chain stake after 70: %s SAN", ledger.UnitsToSAN(h.stakeUnits(h.node("A-seed"), b.address)))
	return nil
}

// stepFaucet funds a fresh wallet through the seed's faucet (SAN_FAUCET=1,
// enabled in stepStartDevnet) using the `sanup faucet` CLI, then confirms the
// balance and re-verifies the transaction signature.
func (h *harness) stepFaucet(stdout io.Writer) error {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		return err
	}
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		return err
	}
	seed := h.node("A-seed")
	logf(stdout, "funding fresh wallet %s through the seed faucet", address)

	command := exec.Command(h.sanup, "faucet",
		"--to", address,
		"--amount", "10",
		"--rpc", fmt.Sprintf("http://127.0.0.1:%d", seed.api))
	command.Dir = h.root
	command.Env = h.env
	output, err := command.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			logf(stdout, "    | %s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("sanup faucet failed: %w", err)
	}
	match := regexp.MustCompile(`tx ([0-9a-f]{64})`).FindStringSubmatch(string(output))
	if len(match) < 2 {
		return fmt.Errorf("faucet output has no tx id: %s", strings.TrimSpace(string(output)))
	}
	txID := match[1]

	if err := waitUntil(60*time.Second, "faucet balance 10 SAN", func() bool {
		return h.balanceUnits(seed, address) == 10*sanBase
	}); err != nil {
		return err
	}
	record, err := h.client("A-seed", false).Transaction(txID)
	if err != nil {
		return fmt.Errorf("cannot fetch faucet tx %s: %w", txID, err)
	}
	transaction, _ := record["transaction"].(map[string]any)
	if transaction == nil || !ledger.VerifyTransaction(transaction) {
		return fmt.Errorf("faucet tx %s does not verify", txID)
	}
	if sender, _ := transaction["sender"].(string); sender != seed.identity.PublicKeyHex() {
		return fmt.Errorf("faucet tx sender %v is not the seed identity", transaction["sender"])
	}
	logf(stdout, "faucet tx %s verified; balance is %s SAN",
		txID, ledger.UnitsToSAN(h.balanceUnits(seed, address)))
	return nil
}

func (h *harness) query(contractID, function string, params []any) (any, error) {
	return h.client("A-seed", false).ContractQuery(contractID, function, params)
}
func (h *harness) queryOr(contractID, function string, params []any) any {
	value, err := h.query(contractID, function, params)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return value
}

func (h *harness) nonce(spec *nodeSpec, address string) int64 {
	account, err := h.client(spec.name, false).Account(address)
	if err != nil {
		return -1
	}
	return asInt64(account["nonce"])
}

func asInt64(value any) int64 {
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
