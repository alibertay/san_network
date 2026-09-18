package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const stateFileName = "sanup.json"

// nodeState is the pid/state record stored in the data directory.
type nodeState struct {
	PID            int               `json:"pid"`
	Address        string            `json:"address"`
	PublicKey      string            `json:"public_key"`
	Host           string            `json:"host"`
	APIPort        int               `json:"api_port"`
	P2PPort        int               `json:"p2p_port"`
	PeerPort       int               `json:"peer_port"`
	ControllerPort int               `json:"controller_port"`
	ChainID        string            `json:"chain_id"`
	StartedAt      float64           `json:"started_at"`
	Seed           bool              `json:"seed"`
	LogFile        string            `json:"log_file"`
	GenesisEnv     map[string]string `json:"genesis_env,omitempty"`
}

func statePath(dataDir string) string {
	return filepath.Join(dataDir, stateFileName)
}

func readState(dataDir string) nodeState {
	var state nodeState
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return state
	}
	_ = json.Unmarshal(data, &state)
	return state
}

func saveState(dataDir string, state nodeState) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temp := statePath(dataDir) + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, statePath(dataDir))
}

func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ---------------------------------------------------------------------- #
// Ports
// ---------------------------------------------------------------------- #

type nodePorts struct {
	API        int
	P2P        int
	Peer       int
	Controller int
}

func defaultPorts() nodePorts {
	return nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
}

// selectPorts honours explicit --*-port flags first, then reuses the ports a
// previous invocation recorded for this data directory, then falls back to the
// devnet defaults, scanning upwards for the first free port.
func selectPorts(host string, opts options, state nodeState) (nodePorts, error) {
	requested := defaultPorts()
	if state.APIPort > 0 && state.PeerPort > 0 && state.P2PPort > 0 && state.ControllerPort > 0 {
		requested = nodePorts{
			API:        state.APIPort,
			P2P:        state.P2PPort,
			Peer:       state.PeerPort,
			Controller: state.ControllerPort,
		}
	}
	if opts.apiPort > 0 {
		requested.API = opts.apiPort
	}
	if opts.p2pPort > 0 {
		requested.P2P = opts.p2pPort
	}
	if opts.peerPort > 0 {
		requested.Peer = opts.peerPort
	}
	if opts.controllerPort > 0 {
		requested.Controller = opts.controllerPort
	}

	used := map[int]bool{}
	api, err := pickPort(host, requested.API, used)
	if err != nil {
		return nodePorts{}, fmt.Errorf("--api-port: %w", err)
	}
	p2p, err := pickPort(host, requested.P2P, used)
	if err != nil {
		return nodePorts{}, fmt.Errorf("--p2p-port: %w", err)
	}
	peer, err := pickPort(host, requested.Peer, used)
	if err != nil {
		return nodePorts{}, fmt.Errorf("--peer-port: %w", err)
	}
	controller, err := pickPort(host, requested.Controller, used)
	if err != nil {
		return nodePorts{}, fmt.Errorf("--controller-port: %w", err)
	}
	return nodePorts{API: api, P2P: p2p, Peer: peer, Controller: controller}, nil
}

func pickPort(host string, base int, used map[int]bool) (int, error) {
	for offset := 0; offset < 50; offset++ {
		port := base + offset
		if used[port] || !portFree(host, port) {
			continue
		}
		used[port] = true
		return port, nil
	}
	return 0, fmt.Errorf("no free port found starting at %d", base)
}

func portFree(host string, port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// ---------------------------------------------------------------------- #
// Child process
// ---------------------------------------------------------------------- #

// childExecutable stages a copy of this binary inside the data directory so
// the detached child no longer holds the temporary executable that "go run"
// wants to delete when the launcher exits.
func childExecutable(dataDir string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	target := filepath.Join(dataDir, "sanup-node")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	sourceAbsolute, sourceErr := filepath.Abs(source)
	targetAbsolute, targetErr := filepath.Abs(target)
	if sourceErr == nil && targetErr == nil && strings.EqualFold(sourceAbsolute, targetAbsolute) {
		return target, nil
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if targetInfo, err := os.Stat(target); err == nil &&
		targetInfo.Size() == sourceInfo.Size() && targetInfo.ModTime().Equal(sourceInfo.ModTime()) {
		return target, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	temp := target + ".tmp"
	if err := os.WriteFile(temp, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		if _, statErr := os.Stat(target); statErr == nil {
			return target, nil
		}
		return "", err
	}
	return target, nil
}

// spawnChild re-executes this binary in child mode (SANUP_CHILD=1) with the
// SAN_* environment prepared by the parent. The child is detached so it keeps
// running after the launcher exits.
func spawnChild(executable string, env []string, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	command := exec.Command(executable)
	command.Env = env
	command.Stdout = logFile
	command.Stderr = logFile
	command.Stdin = nil
	command.SysProcAttr = childSysProcAttr()
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	go func() { _ = command.Wait() }()
	return pid, nil
}

// ---------------------------------------------------------------------- #
// Health
// ---------------------------------------------------------------------- #

func nodeHealthy(host string, port int, timeout time.Duration) bool {
	return nodeHealthyAuth(host, port, timeout, "")
}

// nodeHealthyAuth is nodeHealthy with the optional bearer token.
func nodeHealthyAuth(host string, port int, timeout time.Duration, token string) bool {
	if port <= 0 {
		return false
	}
	client := newHealthClient(host, port, timeout, token)
	health, err := client.Health()
	if err != nil {
		return false
	}
	status, _ := health["status"].(string)
	return status == "ok"
}

func waitHealthy(host string, port int, timeoutSeconds float64, token string) error {
	deadline := time.Now().Add(time.Duration(timeoutSeconds * float64(time.Second)))
	lastError := "not ready"
	for time.Now().Before(deadline) {
		client := newHealthClient(host, port, 2*time.Second, token)
		health, err := client.Health()
		if err == nil {
			if status, _ := health["status"].(string); status == "ok" {
				return nil
			}
		} else {
			lastError = err.Error()
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the node did not become healthy within %.0fs (%s)", timeoutSeconds, lastError)
}

// envList applies the overrides on top of the parent environment, replacing
// entries with the same name.
func envList(overrides map[string]string) []string {
	values := []string{}
	applied := map[string]bool{}
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if replacement, ok := overrides[name]; ok {
			values = append(values, name+"="+replacement)
			applied[name] = true
			continue
		}
		values = append(values, entry)
	}
	for name, value := range overrides {
		if !applied[name] {
			values = append(values, name+"="+value)
		}
	}
	return values
}

func atoiOr(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}
