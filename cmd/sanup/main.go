// Command sanup is the one-command devnet launcher for the Go SAN node. It
// creates (or reuses) the node key, picks free local ports, publishes the node
// in the shared peer registry so other local nodes discover it automatically,
// starts the node detached, waits for /health and reconciles --stake through
// the node's own REST API.
//
//	sanup --wallet 0x... --stake 100
//	sanup --seeds seed.example.com --advertise-host vps.example.com
//	sanup cert --advertise-host vps.example.com
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
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/sanlog"
	"github.com/alibertay/san_network/internal/sdk"
	"github.com/alibertay/san_network/internal/version"
)

const usageText = `usage: sanup [options]
       sanup cert [--dir DIR] [--advertise-host HOSTS]
       sanup faucet --to ADDRESS [--amount SAN]

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
  --seeds LIST            seed hosts (DNS names and/or host:peer-port), comma
                          separated; used for wide-area peer discovery
  --advertise-host HOST   public IP/DNS name announced to peers
  --peer-cache FILE       address-manager cache (default <data-dir>/peers-cache.json)
  --registry PATH         local peer registry file (default ~/.san/peers.json)
  --no-registry           disable the same-machine peer registry entirely
  --tls-cert FILE         node TLS certificate (PEM)
  --tls-key FILE          node TLS private key (PEM)
  --tls-ca FILE           CA bundle used to verify peers
  --api-host HOST         REST bind host (default 0.0.0.0)
  --api-token TOKEN       require Authorization: Bearer TOKEN on the REST API
  --faucet                enable the faucet endpoint (SAN_FAUCET=1)
  --faucet-amount SAN     default amount per request (default 10)
  --faucet-max SAN        maximum amount per request (default 100)
  --faucet-cooldown SEC   per-address/per-IP cooldown (default 60)
  --seed[=auto|true|false] start as founder (default auto: seed when the peer
                          registry has no reachable node, otherwise join)
  --reward-address ADDR   block reward address (default: the wallet)
  --genesis-amount SAN    founder premine for the wallet (default 10000)
  --host HOST             P2P bind host (default 127.0.0.1; use 0.0.0.0 public)
  --chain-id ID           chain id (default san-devnet-1)
  --timeout SECONDS       seconds to wait for health (default 90)
  --foreground            run the node in this process instead of detaching
                          (for systemd Type=simple; SIGTERM stops it gracefully)
  --status                show address, height, finalized height, peers,
                          balance and stake
  --stop                  stop the node started from this data directory
  --json                  print --status output as JSON

Every option can also come from the SAN_* environment (SAN_HOST, SAN_API_PORT,
SAN_ADVERTISE_HOST, SAN_DNS_SEEDS, SAN_BOOTSTRAP, SAN_TLS_CERT/KEY/CA,
SAN_API_TOKEN, SAN_DB_BACKEND, SAN_DB_PATH, SAN_PEERS_CACHE, SAN_SEED,
SAN_STAKE, SAN_GENESIS_AMOUNT, ...); explicit flags win.

examples:
  go run ./cmd/sanup --wallet 0x... --stake 100
  go run ./cmd/sanup cert --advertise-host vps.example.com
  go run ./cmd/sanup --host 0.0.0.0 --advertise-host vps.example.com \
      --seeds seed.example.com --stake 100
  go run ./cmd/sanup --faucet --faucet-amount 10 --faucet-max 100
  go run ./cmd/sanup faucet --to 0x... --amount 10
  go run ./cmd/sanup --foreground --data-dir /var/lib/san   # systemd
  go run ./cmd/sanup --status
  go run ./cmd/sanup --stop
`

type seedChoice struct {
	value    string
	explicit bool
}

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

// normalize validates the value coming from SAN_SEED.
func (choice *seedChoice) normalize() error {
	return choice.Set(choice.value)
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
	seeds          string
	advertiseHost  string
	peerCache      string
	registryPath   string
	noRegistry     bool
	tlsCert        string
	tlsKey         string
	tlsCA          string
	apiHost        string
	apiToken       string
	faucet         bool
	faucetAmount   string
	faucetMax      string
	faucetCooldown string
	stop           bool
	status         bool
	seed           seedChoice
	rewardAddress  string
	genesisAmount  string
	host           string
	chainID        string
	timeout        float64
	foreground     bool
	json           bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	if _, err := sanlog.ConfigureFromEnv(); err != nil {
		fmt.Fprintf(stderr, "sanup: %v\n", err)
	}
	if len(argv) > 0 && argv[0] == "version" {
		fmt.Fprintln(stdout, version.Resolve(netnode.ProtocolVersion, ledger.SchemaVersion))
		return 0
	}
	if os.Getenv("SANUP_CHILD") == "1" {
		return runChild(stdout, stderr)
	}
	if len(argv) > 0 && argv[0] == "cert" {
		return runCert(argv[1:], stdout, stderr)
	}
	if len(argv) > 0 && argv[0] == "faucet" {
		return runFaucet(argv[1:], stdout, stderr)
	}
	opts, code := parseOptions(argv, stdout, stderr)
	if code >= 0 {
		return code
	}
	configureAPITLS(opts)
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
		stake:         envDefaultString("SAN_STAKE", ""),
		dataDir:       envDefaultString("SAN_DATA_DIR", filepath.Join("data", "go-node")),
		keyFile:       envDefaultString("SAN_KEY_FILE", ""),
		genesisAmount: envDefaultString("SAN_GENESIS_AMOUNT", "10000"),
		host:          envDefaultString("SAN_HOST", "127.0.0.1"),
		apiHost:       envDefaultString("SAN_API_HOST", "0.0.0.0"),
		chainID:       envDefaultString("SAN_CHAIN_ID", "san-devnet-1"),
		seeds:         envDefaultString("SAN_DNS_SEEDS", ""),
		bootstrap:     envDefaultString("SAN_BOOTSTRAP", ""),
		advertiseHost: envDefaultString("SAN_ADVERTISE_HOST", ""),
		apiToken:      envDefaultString("SAN_API_TOKEN", ""),
		tlsCert:       envDefaultString("SAN_TLS_CERT", ""),
		tlsKey:        envDefaultString("SAN_TLS_KEY", ""),
		tlsCA:         envDefaultString("SAN_TLS_CA", ""),
		peerCache:     envDefaultString("SAN_PEERS_CACHE", ""),
		registryPath:  envDefaultString("SAN_PEER_REGISTRY", ""),
		timeout:       90.0,
	}
	opts.stakeProvided = opts.stake != ""
	opts.apiPort = envDefaultInt("SAN_API_PORT", 0)
	opts.p2pPort = envDefaultInt("SAN_P2P_PORT", 0)
	opts.peerPort = envDefaultInt("SAN_PEER_PORT", 0)
	opts.controllerPort = envDefaultInt("SAN_CONTROLLER_PORT", 0)
	opts.seed.value = envDefaultString("SAN_SEED", "auto")

	flags := flag.NewFlagSet("sanup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usageText) }
	flags.StringVar(&opts.wallet, "wallet", "", "")
	flags.StringVar(&opts.stake, "stake", opts.stake, "")
	flags.StringVar(&opts.dataDir, "data-dir", opts.dataDir, "")
	flags.StringVar(&opts.keyFile, "key-file", opts.keyFile, "")
	flags.IntVar(&opts.apiPort, "api-port", opts.apiPort, "")
	flags.IntVar(&opts.p2pPort, "p2p-port", opts.p2pPort, "")
	flags.IntVar(&opts.peerPort, "peer-port", opts.peerPort, "")
	flags.IntVar(&opts.controllerPort, "controller-port", opts.controllerPort, "")
	flags.StringVar(&opts.bootstrap, "bootstrap", opts.bootstrap, "")
	flags.StringVar(&opts.seeds, "seeds", opts.seeds, "")
	flags.StringVar(&opts.advertiseHost, "advertise-host", opts.advertiseHost, "")
	flags.StringVar(&opts.peerCache, "peer-cache", opts.peerCache, "")
	flags.StringVar(&opts.registryPath, "registry", opts.registryPath, "")
	flags.BoolVar(&opts.noRegistry, "no-registry", false, "")
	flags.StringVar(&opts.tlsCert, "tls-cert", opts.tlsCert, "")
	flags.StringVar(&opts.tlsKey, "tls-key", opts.tlsKey, "")
	flags.StringVar(&opts.tlsCA, "tls-ca", opts.tlsCA, "")
	flags.StringVar(&opts.apiHost, "api-host", opts.apiHost, "")
	flags.StringVar(&opts.apiToken, "api-token", opts.apiToken, "")
	flags.BoolVar(&opts.faucet, "faucet", false, "")
	flags.StringVar(&opts.faucetAmount, "faucet-amount", "", "")
	flags.StringVar(&opts.faucetMax, "faucet-max", "", "")
	flags.StringVar(&opts.faucetCooldown, "faucet-cooldown", "", "")
	flags.BoolVar(&opts.stop, "stop", false, "")
	flags.BoolVar(&opts.status, "status", false, "")
	flags.Var(&opts.seed, "seed", "")
	flags.StringVar(&opts.rewardAddress, "reward-address", "", "")
	flags.StringVar(&opts.genesisAmount, "genesis-amount", opts.genesisAmount, "")
	flags.StringVar(&opts.host, "host", opts.host, "")
	flags.StringVar(&opts.chainID, "chain-id", opts.chainID, "")
	flags.Float64Var(&opts.timeout, "timeout", opts.timeout, "")
	flags.BoolVar(&opts.foreground, "foreground", false, "")
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
	if opts.noRegistry && opts.registryPath != "" {
		fmt.Fprintln(stderr, "sanup: error: --no-registry and --registry are mutually exclusive")
		return opts, 2
	}
	if (opts.tlsCert == "") != (opts.tlsKey == "") {
		fmt.Fprintln(stderr, "sanup: error: --tls-cert and --tls-key must be provided together")
		return opts, 2
	}
	if value := strings.TrimSpace(opts.faucetAmount); value != "" {
		if units, err := ledger.SanToUnits(value); err != nil || units <= 0 {
			fmt.Fprintf(stderr, "sanup: error: invalid --faucet-amount %q (want a positive SAN number)\n", value)
			return opts, 2
		}
	}
	if value := strings.TrimSpace(opts.faucetMax); value != "" {
		if units, err := ledger.SanToUnits(value); err != nil || units <= 0 {
			fmt.Fprintf(stderr, "sanup: error: invalid --faucet-max %q (want a positive SAN number)\n", value)
			return opts, 2
		}
	}
	if value := strings.TrimSpace(opts.faucetCooldown); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed < 0 {
			fmt.Fprintln(stderr, "sanup: error: invalid --faucet-cooldown (want seconds >= 0)")
			return opts, 2
		}
	}
	flags.Visit(func(defined *flag.Flag) {
		if defined.Name == "stake" {
			opts.stakeProvided = true
			return
		}
		if defined.Name == "seed" {
			opts.seed.explicit = true
		}
	})
	// SAN_SEED is only honoured when --seed was not passed explicitly.
	if !opts.seed.explicit {
		if raw := strings.TrimSpace(os.Getenv("SAN_SEED")); raw != "" {
			opts.seed.value = raw
		}
	}
	if err := opts.seed.normalize(); err != nil {
		fmt.Fprintf(stderr, "sanup: error: %v\n", err)
		return opts, 2
	}
	return opts, -1
}

// envDefaultString returns the trimmed environment value or the fallback.
func envDefaultString(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func envDefaultInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
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
	// A previously started TLS node keeps speaking HTTPS even when --status is
	// invoked without the --tls-* flags.
	if state.TLS {
		localAPITLS = true
	}
	host := opts.host
	dial := connectHost(host)
	if state.PID > 0 && processAlive(state.PID) {
		if state.APIPort > 0 && nodeHealthyAuth(connectHost(state.Host), state.APIPort, 1500*time.Millisecond, opts.apiToken) {
			logf(stdout, "node already running (pid %d) at http://%s:%d; reconciling stake only",
				state.PID, connectHost(state.Host), state.APIPort)
			client := newClient(apiURL(connectHost(state.Host), state.APIPort), identity, 15*time.Second, opts.apiToken)
			if targetUnits >= 0 {
				if err := reconcileStake(client, wallet, targetUnits, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
					return 1
				}
			}
			return printStatus(state, wallet, stdout, stderr, opts.json, opts.apiToken)
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
	} else if strings.TrimSpace(bootstrap) != "" {
		logf(stdout, "joining: %s (genesis source; peers via seeds/registry)", bootstrap)
	} else {
		logf(stdout, "joining: peers via wide-area discovery")
	}
	logf(stdout, "p2p    : api=%d p2p=%d peer=%d controller=%d", ports.API, ports.P2P, ports.Peer, ports.Controller)
	logf(stdout, "data   : %s", dataDir)

	newState := nodeState{
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
		TLS:            opts.tlsCert != "" && opts.tlsKey != "",
		LogFile:        logFile,
		GenesisEnv:     genesisEnv,
	}
	baseURL := apiURL(dial, ports.API)

	// reconcileAfterStart waits for /health, applies --stake and prints the
	// status; it is the tail of runStart for both foreground and detached
	// children.
	reconcileAfterStart := func(current nodeState) int {
		if err := waitHealthy(dial, ports.API, opts.timeout, opts.apiToken); err != nil {
			fmt.Fprintf(stderr, "[sanup] error: %v (log %s)\n", err, current.LogFile)
			return 1
		}
		client := newClient(baseURL, identity, 15*time.Second, opts.apiToken)
		if targetUnits >= 0 {
			if err := reconcileStake(client, wallet, targetUnits, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "[sanup] error: %v\n", err)
				return 1
			}
		}
		if code := printStatus(current, wallet, stdout, stderr, opts.json, opts.apiToken); code != 0 {
			return code
		}
		if !opts.json {
			logf(stdout, "next: sancli --rpc %s health", baseURL)
		}
		return 0
	}

	if opts.foreground {
		for _, entry := range env {
			name, value, found := strings.Cut(entry, "=")
			if found {
				_ = os.Setenv(name, value)
			}
		}
		newState.PID = os.Getpid()
		if err := saveState(dataDir, newState); err != nil {
			fmt.Fprintf(stderr, "[sanup] warning: cannot record the node state: %v\n", err)
		}
		current := newState
		logf(stdout, "running sannode in the foreground (pid %d)", current.PID)
		go reconcileAfterStart(current)
		code := runChild(stdout, stderr)
		current.PID = 0
		_ = saveState(dataDir, current)
		return code
	}

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
	newState.PID = pid
	if err := saveState(dataDir, newState); err != nil {
		fmt.Fprintf(stderr, "[sanup] warning: cannot record the node state: %v\n", err)
	}
	logf(stdout, "started sannode (pid %d, log %s)", pid, logFile)

	if err := waitHealthy(dial, ports.API, opts.timeout, opts.apiToken); err != nil {
		fmt.Fprintf(stderr, "[sanup] error: %v; inspect %s\n", err, logFile)
		killProcess(pid)
		if !waitProcessExit(pid, 5*time.Second) {
			forceKillProcess(pid)
			_ = waitProcessExit(pid, 2*time.Second)
		}
		newState.PID = 0
		_ = saveState(dataDir, newState)
		return 1
	}
	return reconcileAfterStart(newState)
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

// localAPITLS mirrors the node's own TLS configuration so the launcher talks
// to its REST API over the right scheme. It is set once per invocation from
// --tls-cert/--tls-key (or the SAN_TLS_* environment).
var (
	localAPITLS bool
	localAPICA  string
)

func configureAPITLS(opts options) {
	localAPITLS = opts.tlsCert != "" && opts.tlsKey != ""
	localAPICA = opts.tlsCA
}

func apiURL(host string, port int) string {
	scheme := "http"
	if localAPITLS {
		scheme = "https"
	}
	return scheme + "://" + host + ":" + strconv.Itoa(port)
}

// newClient builds an SDK client for this node, trusting the devnet CA when
// configured (or skipping verification for the node's own self-signed pair).
func newClient(baseURL string, identity *ledger.NodeIdentity, timeout time.Duration, token string) *sdk.SanClient {
	client := sdk.NewSanClient(baseURL, identity, timeout).SetToken(token)
	if localAPITLS {
		_ = client.SetTLS(localAPICA, strings.TrimSpace(localAPICA) == "")
	}
	return client
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
