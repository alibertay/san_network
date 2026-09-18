// Command sanup is the one-command devnet launcher for the Go SAN node. It
// creates (or reuses) the node key, picks free local ports, publishes the node
// in the shared peer registry so other local nodes discover it automatically,
// starts the node detached, waits for /health and reconciles --stake through
// the node's own REST API.
//
//	sanup --wallet 0x... --stake 100
//	sanup --status
//	sanup --stop
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

const usageText = `usage: sanup [options]

One-command Go SAN devnet node (no Python at runtime).

options:
  --wallet ADDRESS        wallet address (0x...); defaults to the node key address
  --stake SAN             desired on-chain stake in SAN; reconciled on start
                          (omit to leave the current stake untouched)
  --data-dir DIR          node data directory (default data/go-node)
  --key-file FILE         node identity key file (default <data-dir>/san_key.json)
  --api-port PORT         REST port (default 8000, auto-increments when busy)
  --p2p-port PORT         P2P (gRPC) port (default 8765)
  --peer-port PORT        peer session port (default 8770)
  --controller-port PORT  controller port (default 8769)
  --bootstrap LIST        seed REST endpoints host:port, comma separated
  --seed[=auto|true|false] start as founder (default auto: seed when the peer
                          registry has no reachable node, otherwise join)
  --reward-address ADDR   block reward address (default: the wallet)
  --genesis-amount SAN    founder premine for the wallet (default 10000)
  --host HOST             bind host (default 127.0.0.1)
  --chain-id ID           chain id (default san-devnet-1)
  --timeout SECONDS       seconds to wait for health (default 90)
  --status                show address, height, finalized height, peers,
                          balance and stake
  --stop                  stop the node started from this data directory
  --json                  print --status output as JSON

examples:
  go run ./cmd/sanup --wallet 0x... --stake 100
  go run ./cmd/sanup --status
  go run ./cmd/sanup --stop
`

type seedChoice struct{ value string }

func (choice *seedChoice) String() string { return choice.value }

func (choice *seedChoice) Set(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		choice.value = "auto"
	case "true", "yes", "on", "1":
		choice.value = "true"
	case "false", "no", "off", "0":
		choice.value = "false"
	default:
		return fmt.Errorf("invalid --seed %q (want auto, true or false)", value)
	}
	return nil
}

func (choice *seedChoice) IsBoolFlag() bool { return true }

type options struct {
	wallet         string
	stake          string
	stakeProvided  bool
	dataDir        string
	keyFile        string
	apiPort        int
	p2pPort        int
	peerPort       int
	controllerPort int
	bootstrap      string
	stop           bool
	status         bool
	seed           seedChoice
	rewardAddress  string
	genesisAmount  string
	host           string
	chainID        string
	timeout        float64
	json           bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	if os.Getenv("SANUP_CHILD") == "1" {
		return runChild(stdout, stderr)
	}
	opts, code := parseOptions(argv, stdout, stderr)
	if code >= 0 {
		return code
	}
	if opts.stop {
		return runStop(opts, stdout, stderr)
	}
	if opts.status {
		return runStatus(opts, stdout, stderr)
	}
	return runStart(opts, stdout, stderr)
}

func parseOptions(argv []string, stdout, stderr io.Writer) (options, int) {
	opts := options{
		stake:         "",
		dataDir:       filepath.Join("data", "go-node"),
		genesisAmount: "10000",
		host:          "127.0.0.1",
		chainID:       "san-devnet-1",
		timeout:       90.0,
	}
	opts.seed.value = "auto"

	flags := flag.NewFlagSet("sanup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usageText) }
	flags.StringVar(&opts.wallet, "wallet", "", "")
	flags.StringVar(&opts.stake, "stake", "", "")
	flags.StringVar(&opts.dataDir, "data-dir", opts.dataDir, "")
	flags.StringVar(&opts.keyFile, "key-file", "", "")
	flags.IntVar(&opts.apiPort, "api-port", 0, "")
	flags.IntVar(&opts.p2pPort, "p2p-port", 0, "")
	flags.IntVar(&opts.peerPort, "peer-port", 0, "")
	flags.IntVar(&opts.controllerPort, "controller-port", 0, "")
	flags.StringVar(&opts.bootstrap, "bootstrap", "", "")
	flags.BoolVar(&opts.stop, "stop", false, "")
	flags.BoolVar(&opts.status, "status", false, "")
	flags.Var(&opts.seed, "seed", "")
	flags.StringVar(&opts.rewardAddress, "reward-address", "", "")
	flags.StringVar(&opts.genesisAmount, "genesis-amount", opts.genesisAmount, "")
	flags.StringVar(&opts.host, "host", opts.host, "")
	flags.StringVar(&opts.chainID, "chain-id", opts.chainID, "")
	flags.Float64Var(&opts.timeout, "timeout", opts.timeout, "")
	flags.BoolVar(&opts.json, "json", false, "")

	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, 0
		}
		return opts, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sanup: error: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		fmt.Fprint(stderr, usageText)
		return opts, 2
	}
	flags.Visit(func(defined *flag.Flag) {
		if defined.Name == "stake" {
			opts.stakeProvided = true
		}
	})
	return opts, -1
}

// runStart is the default action: start (or reuse) the node and reconcile the
// requested stake.
func runStart(opts options, stdout, stderr io.Writer) int {
	dataDir, keyFile, identity, address, code := resolveIdentity(opts, stdout, stderr, true)
	if code >= 0 {
		return code
	}
	wallet := address
	if opts.wallet != "" {
		normalized, err := ledger.NormalizeAddress(opts.wallet)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] invalid --wallet: %v\n", err)
			return 2
		}
		if normalized != address {
			fmt.Fprintf(stderr,
				"[sanup] --wallet %s does not match the node key file\n"+
					"  key file   : %s\n"+
					"  its address: %s\n"+
					"Use the wallet whose key lives in that file (--key-file), or a different data directory.\n",
				normalized, keyFile, address)
			return 2
		}
		wallet = normalized
	}
	rewardAddress := wallet
	if opts.rewardAddress != "" {
		normalized, err := ledger.NormalizeAddress(opts.rewardAddress)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] invalid --reward-address: %v\n", err)
			return 2
		}
		rewardAddress = normalized
	}
	if _, err := ledger.SanToUnits(opts.genesisAmount); err != nil {
		fmt.Fprintf(stderr, "[sanup] invalid --genesis-amount: %v\n", err)
		return 2
	}

	targetUnits := int64(-1)
	if opts.stakeProvided {
		units, err := ledger.SanToUnits(opts.stake)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] invalid --stake: %v\n", err)
			return 2
		}
		if units < 0 {
			fmt.Fprintf(stderr, "[sanup] --stake cannot be negative\n")
			return 2
		}
		targetUnits = units
	}

	state := readState(dataDir)
	host := opts.host
	dial := connectHost(host)
	if state.PID > 0 && processAlive(state.PID) {
		if state.APIPort > 0 && nodeHealthy(connectHost(state.Host), state.APIPort, 1500*time.Millisecond) {
			logf(stdout, "node already running (pid %d) at http://%s:%d; reconciling stake only",
				state.PID, connectHost(state.Host), state.APIPort)
			client := sdk.NewSanClient(apiURL(connectHost(state.Host), state.APIPort), identity, 15*time.Second)
			if targetUnits >= 0 {
				if err := reconcileStake(client, wallet, targetUnits, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
					return 1
				}
			}
			return printStatus(state, wallet, stdout, stderr, opts.json)
		}
		fmt.Fprintf(stderr,
			"[sanup] a node process (pid %d) already exists for %s but does not answer on http://%s:%d\n"+
				"        inspect %s or stop it with: sanup --data-dir %s --stop\n",
			state.PID, dataDir, connectHost(state.Host), state.APIPort, state.LogFile, dataDir)
		return 1
	}

	ports, err := selectPorts(host, opts, state)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
		return 1
	}
	seed, bootstrap, genesisEnv, err := resolveGenesis(opts, identity.PublicKeyHex())
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
		return 1
	}

	logFile := filepath.Join(dataDir, "node.log")
	env := buildChildEnv(opts, dataDir, keyFile, identity.PublicKeyHex(), rewardAddress, ports, seed, bootstrap, genesisEnv)
	logf(stdout, "wallet : %s", wallet)
	if seed {
		logf(stdout, "founder: premine %s SAN to %s", opts.genesisAmount, wallet)
	} else {
		logf(stdout, "joining: %s (auto-discovered through the peer registry)", bootstrap)
	}
	logf(stdout, "p2p    : api=%d p2p=%d peer=%d controller=%d", ports.API, ports.P2P, ports.Peer, ports.Controller)
	logf(stdout, "data   : %s", dataDir)

	nodeExecutable, err := childExecutable(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot stage the node executable: %v\n", err)
		return 1
	}
	pid, err := spawnChild(nodeExecutable, env, logFile)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot start the node: %v\n", err)
		return 1
	}
	newState := nodeState{
		PID:            pid,
		Address:        wallet,
		PublicKey:      identity.PublicKeyHex(),
		Host:           host,
		APIPort:        ports.API,
		P2PPort:        ports.P2P,
		PeerPort:       ports.Peer,
		ControllerPort: ports.Controller,
		ChainID:        opts.chainID,
		StartedAt:      nowSeconds(),
		Seed:           seed,
		LogFile:        logFile,
	}
	if err := saveState(dataDir, newState); err != nil {
		fmt.Fprintf(stderr, "[sanup] warning: cannot record the node state: %v\n", err)
	}
	logf(stdout, "started sannode (pid %d, log %s)", pid, logFile)

	baseURL := apiURL(dial, ports.API)
	if err := waitHealthy(dial, ports.API, opts.timeout); err != nil {
		fmt.Fprintf(stderr, "[sanup] error: %v; inspect %s\n", err, logFile)
		killProcess(pid)
		newState.PID = 0
		_ = saveState(dataDir, newState)
		return 1
	}

	client := sdk.NewSanClient(baseURL, identity, 15*time.Second)
	if targetUnits >= 0 {
		if err := reconcileStake(client, wallet, targetUnits, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
			return 1
		}
	}
	if code := printStatus(newState, wallet, stdout, stderr, opts.json); code != 0 {
		return code
	}
	if opts.json {
		return 0
	}
	logf(stdout, "next: sancli --rpc %s health", baseURL)
	return 0
}

// resolveIdentity resolves the data dir and key file and loads (or generates)
// the node identity. allowGenerate is false for --status.
func resolveIdentity(opts options, stdout, stderr io.Writer, allowGenerate bool) (string, string, *ledger.NodeIdentity, string, int) {
	dataDir, err := filepath.Abs(opts.dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] invalid --data-dir: %v\n", err)
		return "", "", nil, "", 2
	}
	keyFile := opts.keyFile
	if keyFile == "" {
		keyFile = filepath.Join(dataDir, "san_key.json")
	}
	if keyFile, err = filepath.Abs(keyFile); err != nil {
		fmt.Fprintf(stderr, "[sanup] invalid --key-file: %v\n", err)
		return "", "", nil, "", 2
	}
	var identity *ledger.NodeIdentity
	if !allowGenerate {
		if _, statErr := os.Stat(keyFile); statErr != nil {
			return dataDir, keyFile, nil, "", -1
		}
		identity, err = ledger.IdentityFromFile(keyFile)
	} else {
		identity, err = ledger.LoadIdentity(keyFile, true)
	}
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot load the node key %s: %v\n", keyFile, err)
		return "", "", nil, "", 1
	}
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot derive the wallet address: %v\n", err)
		return "", "", nil, "", 1
	}
	return dataDir, keyFile, identity, address, -1
}

func apiURL(host string, port int) string {
	return "http://" + host + ":" + strconv.Itoa(port)
}

func connectHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

func logf(stdout io.Writer, format string, args ...any) {
	fmt.Fprintf(stdout, "[sanup] "+format+"\n", args...)
}
