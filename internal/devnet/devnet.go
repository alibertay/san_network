// Package devnet is the shared local multi-node harness for the Go SAN tools.
// It builds cmd/sanup once, allocates free ports, creates identities and
// starts/stops nodes through sanup (the same path cmd/sane2e exercises).
//
// It is used by cmd/sansoak (soak activity and invariants) and by
// cmd/sanbench (local propagation/sync measurements). It is not used by the
// long-running node itself.
package devnet

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

// NodeSpec describes one local node. URL is set in RPC mode (target an
// already running node instead of spawning one).
type NodeSpec struct {
	Name           string
	URL            string
	Dir            string
	KeyFile        string
	Identity       *ledger.NodeIdentity
	Address        string
	APIPort        int
	P2PPort        int
	PeerPort       int
	ControllerPort int
	Running        bool
}

// Harness owns the temporary directories, the sanup binary and the nodes.
type Harness struct {
	Root     string
	Temp     string
	Sanup    string
	Registry string
	DataRoot string
	Env      []string
	Nodes    []*NodeSpec
	Keep     bool
	Logf     func(format string, args ...any)
}

// New builds sanup and prepares the temporary harness directories.
func New(root, tempBase string, logf func(format string, args ...any)) (*Harness, error) {
	if root == "" {
		discovered, err := findRepoRoot()
		if err != nil {
			return nil, err
		}
		root = discovered
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	temp, err := os.MkdirTemp(tempBase, "sandevnet-*")
	if err != nil {
		return nil, err
	}
	h := &Harness{
		Root:     root,
		Temp:     temp,
		Registry: filepath.Join(temp, "peers.json"),
		DataRoot: filepath.Join(temp, "data"),
		Env:      append(os.Environ(), "SAN_PEER_REGISTRY="+filepath.Join(temp, "peers.json")),
		Logf:     logf,
	}
	executable := filepath.Join(temp, "sanup")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-o", executable, "./cmd/sanup")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(temp)
		return nil, fmt.Errorf("go build ./cmd/sanup failed: %v\n%s", err, strings.TrimSpace(string(output)))
	}
	h.Sanup = executable
	return h, nil
}

// AddNodes creates count node identities and port assignments.
func (h *Harness) AddNodes(count int) error {
	ports, err := allocatePorts(count * 4)
	if err != nil {
		return err
	}
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("N%d", index)
		dir := filepath.Join(h.DataRoot, strings.ToLower(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		spec := &NodeSpec{
			Name:           name,
			Dir:            dir,
			KeyFile:        filepath.Join(dir, "san_key.json"),
			APIPort:        ports[index*4],
			P2PPort:        ports[index*4+1],
			PeerPort:       ports[index*4+2],
			ControllerPort: ports[index*4+3],
		}
		identity, err := ledger.LoadIdentity(spec.KeyFile, true)
		if err != nil {
			return fmt.Errorf("cannot create the %s key: %w", name, err)
		}
		spec.Identity = identity
		spec.Address, err = ledger.AddressFromPublicKey(identity.PublicKey)
		if err != nil {
			return err
		}
		h.Nodes = append(h.Nodes, spec)
	}
	return nil
}

// Node returns the named node spec (nil when unknown).
func (h *Harness) Node(name string) *NodeSpec {
	for _, spec := range h.Nodes {
		if spec.Name == name {
			return spec
		}
	}
	return nil
}

// RunNode invokes sanup for a node; extra arguments follow the standard set.
func (h *Harness) RunNode(spec *NodeSpec, extra ...string) error {
	args := []string{
		"--data-dir", spec.Dir,
		"--key-file", spec.KeyFile,
		"--api-port", strconv.Itoa(spec.APIPort),
		"--p2p-port", strconv.Itoa(spec.P2PPort),
		"--peer-port", strconv.Itoa(spec.PeerPort),
		"--controller-port", strconv.Itoa(spec.ControllerPort),
	}
	args = append(args, extra...)
	command := exec.Command(h.Sanup, args...)
	command.Dir = h.Root
	command.Env = h.Env
	output, err := command.CombinedOutput()
	h.logOutput(string(output))
	if err != nil {
		return fmt.Errorf("sanup %s failed: %v", strings.Join(extra, " "), err)
	}
	spec.Running = true
	return nil
}

// StopNode stops a node started from its data directory.
func (h *Harness) StopNode(spec *NodeSpec) error {
	err := h.RunNode(spec, "--stop")
	spec.Running = false
	return err
}

// StartSeed starts spec as the chain founder plus faucet.
func (h *Harness) StartSeed(spec *NodeSpec) error {
	return h.RunNode(spec, "--seed", "--stake", "0", "--faucet")
}

// StartJoiner starts spec as a joining node (discovery via the local
// registry / wide-area seeds).
func (h *Harness) StartJoiner(spec *NodeSpec) error {
	return h.RunNode(spec)
}

// Client builds an SDK client for a node.
func (h *Harness) Client(spec *NodeSpec, signing bool) *sdk.SanClient {
	identity := (*ledger.NodeIdentity)(nil)
	if signing {
		identity = spec.Identity
	}
	baseURL := spec.URL
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", spec.APIPort)
	}
	return sdk.NewSanClient(strings.TrimRight(baseURL, "/"), identity, 15*time.Second)
}

// WaitHealthy waits for the node's /health status.
func (h *Harness) WaitHealthy(spec *NodeSpec, timeout time.Duration) error {
	return WaitUntil(timeout, fmt.Sprintf("%s to become healthy", spec.Name), func() bool {
		health, err := h.Client(spec, false).Health()
		if err != nil {
			return false
		}
		status, _ := health["status"].(string)
		return status == "ok"
	})
}

// WaitHeight waits until the node reports at least height.
func (h *Harness) WaitHeight(spec *NodeSpec, height int64, timeout time.Duration) error {
	return WaitUntil(timeout, fmt.Sprintf("%s to reach height %d", spec.Name, height), func() bool {
		health, err := h.Client(spec, false).Health()
		if err != nil {
			return false
		}
		return ToInt64(health["height"]) >= height
	})
}

// WaitPeers waits until the node sees at least count peers.
func (h *Harness) WaitPeers(spec *NodeSpec, count int64, timeout time.Duration) error {
	return WaitUntil(timeout, fmt.Sprintf("%s to see %d peer(s)", spec.Name, count), func() bool {
		health, err := h.Client(spec, false).Health()
		if err != nil {
			return false
		}
		return ToInt64(health["peers"]) >= count
	})
}

// WaitSynced waits until spec reaches the seed's height (node 0).
func (h *Harness) WaitSynced(spec *NodeSpec, timeout time.Duration) error {
	if len(h.Nodes) == 0 {
		return fmt.Errorf("no nodes")
	}
	seed := h.Nodes[0]
	return WaitUntil(timeout, fmt.Sprintf("%s to sync with %s", spec.Name, seed.Name), func() bool {
		peerHeight := int64(-1)
		if health, err := h.Client(seed, false).Health(); err == nil {
			peerHeight = ToInt64(health["height"])
		}
		health, err := h.Client(spec, false).Health()
		if err != nil {
			return false
		}
		return peerHeight >= 0 && ToInt64(health["height"]) >= peerHeight
	})
}

// Cleanup stops every node and removes the temporary directory (unless Keep).
func (h *Harness) Cleanup() {
	if h == nil {
		return
	}
	for _, spec := range h.Nodes {
		if spec.Running {
			_ = h.runStop(spec)
		}
	}
	if h.Temp != "" {
		if h.Keep {
			h.Logf("keeping %s (--keep)", h.Temp)
		} else {
			_ = os.RemoveAll(h.Temp)
		}
	}
}

func (h *Harness) runStop(spec *NodeSpec) error {
	args := []string{
		"--data-dir", spec.Dir,
		"--key-file", spec.KeyFile,
		"--api-port", strconv.Itoa(spec.APIPort),
		"--stop",
	}
	command := exec.Command(h.Sanup, args...)
	command.Dir = h.Root
	command.Env = h.Env
	output, _ := command.CombinedOutput()
	_ = output
	spec.Running = false
	return nil
}

func (h *Harness) logOutput(output string) {
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			h.Logf("    | %s", line)
		}
	}
}

// ---------------------------------------------------------------------- #
// helpers
// ---------------------------------------------------------------------- #

// WaitUntil polls condition until timeout.
func WaitUntil(timeout time.Duration, description string, condition func() bool) error {
	if WaitFor(timeout, condition) {
		return nil
	}
	return fmt.Errorf("timed out after %.0fs waiting for %s", timeout.Seconds(), description)
}

// WaitFor polls condition until timeout, returning the final result.
func WaitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return condition()
}

// PortOpen reports whether a local TCP port accepts connections.
func PortOpen(port int) bool {
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func allocatePorts(count int) ([]int, error) {
	ports := make([]int, 0, count)
	for port := 18100; port < 19500 && len(ports) < count; port++ {
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

// ToInt64 converts common JSON number shapes to int64.
func ToInt64(value any) int64 {
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

// WriteLine is a small io.Writer adapter for progress helpers.
func WriteLine(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
