// Command sane2e is the Go end-to-end verification of the SAN devnet: three
// nodes started through cmd/sanup with automatic peer discovery (no Python at
// runtime), SAN transfers, a SANRC20 token, a custom contract and the stake
// reconciliation 0 -> 100 -> 70. It exits non-zero when any step fails.
//
//	go run ./cmd/sane2e
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

const sanBase = int64(ledger.SANBase)

type nodeSpec struct {
	name       string
	dir        string
	key        string
	address    string
	identity   *ledger.NodeIdentity
	api        int
	p2p        int
	peer       int
	controller int
}

type stepResult struct {
	name   string
	passed bool
	detail string
}

type harness struct {
	root     string
	temp     string
	sanup    string
	registry string
	dataRoot string
	env      []string
	nodes    []*nodeSpec
	results  []stepResult
}

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

func run(stdout, stderr io.Writer) int {
	started := time.Now()
	logf(stdout, "SAN Network Go e2e check (cmd/sane2e)")

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(stderr, "sane2e: %v\n", err)
		return 1
	}
	h := &harness{root: root}
	failures := 0
	if err := h.setup(); err != nil {
		fmt.Fprintf(stderr, "sane2e: setup failed: %v\n", err)
		h.cleanup()
		return 1
	}
	defer h.cleanup()

	for _, step := range []struct {
		name string
		run  func(io.Writer) error
	}{
		{"0. three-node devnet with auto-discovery", h.stepStartDevnet},
		{"1. wallet transfer", h.stepTransfers},
		{"2. SANRC20 token", h.stepSANRC20},
		{"3. custom contract", h.stepCustomContract},
		{"4. stake 0 -> 100 -> 70", h.stepStake},
		{"5. faucet funds a fresh wallet", h.stepFaucet},
	} {
		logf(stdout, "\n=== %s ===", step.name)
		err := step.run(stdout)
		if err != nil {
			h.results = append(h.results, stepResult{name: step.name, passed: false, detail: err.Error()})
			logf(stdout, "FAIL %s: %v", step.name, err)
			continue
		}
		h.results = append(h.results, stepResult{name: step.name, passed: true})
		logf(stdout, "PASS %s", step.name)
	}

	logf(stdout, "\n=== summary ===")
	for _, result := range h.results {
		status := "PASS"
		if !result.passed {
			status = "FAIL"
			failures++
		}
		detail := ""
		if result.detail != "" {
			detail = " - " + result.detail
		}
		fmt.Fprintf(stdout, "%s %s%s\n", status, result.name, detail)
	}
	logf(stdout, "%d passed, %d failed in %.1fs", len(h.results)-failures, failures, time.Since(started).Seconds())
	if failures > 0 {
		return 1
	}
	return 0
}

func (h *harness) setup() error {
	temp, err := os.MkdirTemp("", "sane2e-*")
	if err != nil {
		return err
	}
	h.temp = temp
	h.registry = filepath.Join(temp, "peers.json")
	h.dataRoot = filepath.Join(temp, "data")
	h.env = append(os.Environ(), "SAN_PEER_REGISTRY="+h.registry)

	executable := filepath.Join(temp, "sanup")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-o", executable, "./cmd/sanup")
	build.Dir = h.root
	if output, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/sanup failed: %v\n%s", err, strings.TrimSpace(string(output)))
	}
	h.sanup = executable

	ports, err := allocatePorts(12)
	if err != nil {
		return err
	}
	for index, definition := range []struct {
		name string
	}{
		{"A-seed"}, {"B"}, {"C"},
	} {
		spec := &nodeSpec{
			name:       definition.name,
			dir:        filepath.Join(h.dataRoot, strings.ToLower(string(definition.name[0]))),
			api:        ports[index*4],
			p2p:        ports[index*4+1],
			peer:       ports[index*4+2],
			controller: ports[index*4+3],
		}
		spec.key = filepath.Join(spec.dir, "san_key.json")
		if err := os.MkdirAll(spec.dir, 0o755); err != nil {
			return err
		}
		identity, err := ledger.LoadIdentity(spec.key, true)
		if err != nil {
			return fmt.Errorf("cannot create the %s key: %w", spec.name, err)
		}
		spec.identity = identity
		spec.address, err = ledger.AddressFromPublicKey(identity.PublicKey)
		if err != nil {
			return err
		}
		h.nodes = append(h.nodes, spec)
	}
	logf(os.Stdout, "wallets: A=%s B=%s C=%s", h.node("A-seed").address, h.node("B").address, h.node("C").address)
	return nil
}

func (h *harness) node(name string) *nodeSpec {
	for _, spec := range h.nodes {
		if spec.name == name {
			return spec
		}
	}
	return nil
}

func (h *harness) cleanup() {
	if h == nil || h.sanup == "" {
		return
	}
	logf(os.Stdout, "\n=== cleanup ===")
	for _, spec := range h.nodes {
		_ = h.runNode(spec, "--stop")
	}
	waitFor(10*time.Second, "all node ports to close", func() bool {
		for _, spec := range h.nodes {
			if portOpen(spec.api) {
				return false
			}
		}
		return true
	})
	if h.temp != "" {
		// SAN_E2E_KEEP=1 preserves the node data directories and logs for
		// debugging a failed run.
		if os.Getenv("SAN_E2E_KEEP") == "1" {
			logf(os.Stdout, "keeping %s (SAN_E2E_KEEP=1)", h.temp)
		} else {
			_ = os.RemoveAll(h.temp)
		}
	}
}

func (h *harness) runNode(spec *nodeSpec, extra ...string) error {
	args := []string{
		"--data-dir", spec.dir,
		"--key-file", spec.key,
		"--api-port", strconv.Itoa(spec.api),
		"--p2p-port", strconv.Itoa(spec.p2p),
		"--peer-port", strconv.Itoa(spec.peer),
		"--controller-port", strconv.Itoa(spec.controller),
	}
	args = append(args, extra...)
	command := exec.Command(h.sanup, args...)
	command.Dir = h.root
	command.Env = h.env
	output, err := command.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			logf(os.Stdout, "    | %s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("sanup %s failed: %v", strings.Join(extra, " "), err)
	}
	return nil
}

func (h *harness) client(name string, signing bool) *sdk.SanClient {
	spec := h.node(name)
	identity := (*ledger.NodeIdentity)(nil)
	if signing {
		identity = spec.identity
	}
	return sdk.NewSanClient(fmt.Sprintf("http://127.0.0.1:%d", spec.api), identity, 15*time.Second)
}

func allocatePorts(count int) ([]int, error) {
	ports := make([]int, 0, count)
	for port := 18100; port < 19100 && len(ports) < count; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = listener.Close()
		ports = append(ports, port)
	}
	if len(ports) < count {
		return nil, fmt.Errorf("cannot allocate %d free local ports", count)
	}
	return ports, nil
}

func findRepoRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		directory = parent
	}
}

func portOpen(port int) bool {
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func waitFor(timeout time.Duration, description string, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return condition()
}

func waitUntil(timeout time.Duration, description string, condition func() bool) error {
	if waitFor(timeout, description, condition) {
		return nil
	}
	return fmt.Errorf("timed out after %.0fs waiting for %s", timeout.Seconds(), description)
}

func (h *harness) waitHealthy(spec *nodeSpec, timeout time.Duration) error {
	return waitUntil(timeout, fmt.Sprintf("%s to become healthy", spec.name), func() bool {
		health, err := h.client(spec.name, false).Health()
		if err != nil {
			return false
		}
		status, _ := health["status"].(string)
		return status == "ok"
	})
}

func (h *harness) waitHeight(spec *nodeSpec, height int64, timeout time.Duration) error {
	return waitUntil(timeout, fmt.Sprintf("%s to reach height %d", spec.name, height), func() bool {
		health, err := h.client(spec.name, false).Health()
		if err != nil {
			return false
		}
		return asInt64(health["height"]) >= height
	})
}

func (h *harness) waitPeers(spec *nodeSpec, peers int64, timeout time.Duration) error {
	return waitUntil(timeout, fmt.Sprintf("%s to see %d peer(s)", spec.name, peers), func() bool {
		health, err := h.client(spec.name, false).Health()
		if err != nil {
			return false
		}
		return asInt64(health["peers"]) >= peers
	})
}

func (h *harness) waitSynced(spec *nodeSpec, timeout time.Duration) error {
	seedHeight := func() int64 {
		health, err := h.client("A-seed", false).Health()
		if err != nil {
			return -1
		}
		return asInt64(health["height"])
	}
	return waitUntil(timeout, fmt.Sprintf("%s to sync with the seed", spec.name), func() bool {
		health, err := h.client(spec.name, false).Health()
		if err != nil {
			return false
		}
		return asInt64(health["height"]) >= seedHeight()
	})
}

func (h *harness) balanceUnits(spec *nodeSpec, address string) int64 {
	account, err := h.client(spec.name, false).Account(address)
	if err != nil {
		return -1
	}
	return asInt64(account["balance_units"])
}

func (h *harness) stakeUnits(spec *nodeSpec, address string) int64 {
	record, err := h.client(spec.name, false).Stake(address)
	if err != nil {
		return -1
	}
	return asInt64(record["stake_units"])
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
