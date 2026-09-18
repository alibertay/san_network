// Command sannode is the Go port of scripts/run_node.py (one-command devnet
// node) and run.py (single-node entry point). Run it with "--address 0x..."
// and the flags from run_node.py, or use "sannode serve" for the plain
// run.py behavior.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/sanlog"
	"github.com/alibertay/san_network/internal/sdk"
	"github.com/alibertay/san_network/internal/version"
)

const runNodeUsage = `usage: sannode --address ADDRESS [options]

One-command devnet node (port of scripts/run_node.py).

options:
  --address ADDRESS            reward address (0x...)
  --key FILE                   node identity key file (default node_key.json)
  --chain-id ID                chain id (default san-devnet-1)
  --api-port PORT              REST port (default 8000)
  --db FILE                    LMDB database file (default data/<key>.kv)
  --bootstrap HOST:PORT        seed REST endpoint host:port
  --expect-genesis-hash HASH   pin the expected genesis hash
  --genesis-amount AMOUNT      SAN premined to this node in founder mode
	--genesis-alloc ALLOC        shared allocation (overrides fetching)
	--genesis-file FILE          canonical genesis JSON (chain id, allocations,
	                             parameters, fingerprint); SAN_GENESIS_FILE
	--stake AMOUNT               validator deposit in SAN (0 = skip)
  --block-reward AMOUNT        SAN minted per block (founder only)
  --host HOST                  bind host (default 127.0.0.1)
`

// GenesisParameterEnv mirrors scripts/run_node.py GENESIS_PARAMETER_ENV.
var genesisParameterEnv = []string{
	"SAN_CHAIN_ID",
	"SAN_GENESIS_ALLOCATION",
	"SAN_BLOCK_REWARD",
	"SAN_MIN_VALIDATOR_STAKE",
	"SAN_UNBONDING_PERIOD",
	"SAN_SLASH_BPS",
	"SAN_BLOCK_GAS_LIMIT",
	"SAN_PROPOSER_TIMEOUT",
	"SAN_MIN_BLOCK_INTERVAL_MS",
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

func runMain(argv []string, stdout, stderr io.Writer) int {
	if _, err := sanlog.ConfigureFromEnv(); err != nil {
		fmt.Fprintf(stderr, "sannode: %v\n", err)
	}
	if len(argv) > 0 && argv[0] == "version" {
		fmt.Fprintln(stdout, version.Resolve(netnode.ProtocolVersion, ledger.SchemaVersion))
		return 0
	}
	if len(argv) > 0 && argv[0] == "serve" {
		return runServe(argv[1:], stdout, stderr)
	}
	return runNode(argv, stdout, stderr)
}

// runNode mirrors scripts/run_node.py main().
func runNode(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sannode", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, runNodeUsage) }
	address := flags.String("address", "", "")
	key := flags.String("key", "node_key.json", "")
	chainID := flags.String("chain-id", "san-devnet-1", "")
	apiPortFlag := flags.Int("api-port", 0, "")
	dbFlag := flags.String("db", "", "")
	bootstrap := flags.String("bootstrap", "", "")
	expectGenesisHash := flags.String("expect-genesis-hash", "", "")
	genesisAmount := flags.String("genesis-amount", "1000000", "")
	genesisAlloc := flags.String("genesis-alloc", "", "")
	genesisFile := flags.String("genesis-file", "", "")
	stake := flags.Float64("stake", 1000.0, "")
	blockReward := flags.Float64("block-reward", 2.0, "")
	host := flags.String("host", "127.0.0.1", "")
	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *address == "" {
		fmt.Fprintln(stderr, "sannode: error: --address is required")
		fmt.Fprint(stderr, runNodeUsage)
		return 2
	}

	normalizedAddress, err := ledger.NormalizeAddress(*address)
	if err != nil {
		fmt.Fprintf(stderr, "[run_node] invalid --address: %v\n", err)
		return 2
	}

	identity, err := ledger.LoadIdentity(*key, true)
	if err != nil {
		fmt.Fprintf(stderr, "[run_node] cannot load identity %s: %v\n", *key, err)
		return 2
	}

	apiPort := *apiPortFlag
	if apiPort == 0 {
		apiPort = envIntDefault("SAN_API_PORT", 8000)
	}
	dbPath := *dbFlag
	if dbPath == "" {
		dbPath = filepath.Join("data", keyStem(*key)+".kv")
	}

	allocation := *genesisAlloc
	var payload map[string]any
	if *bootstrap != "" {
		if conflicting := checkGenesisEnvironment(); len(conflicting) > 0 {
			fmt.Fprintf(stderr,
				"[run_node] these environment variables would override the seed's "+
					"genesis parameters: %s. Unset them or pass "+
					"--genesis-alloc explicitly.\n", strings.Join(conflicting, ", "))
			return 1
		}
		if allocation == "" || *expectGenesisHash != "" {
			payload = fetchGenesis(*bootstrap, 5*time.Second, stdout)
			if payload == nil && allocation == "" {
				fmt.Fprintln(stderr,
					"[run_node] joiner mode needs the seed's genesis: pass "+
						"--genesis-alloc or make the seed's REST endpoint reachable")
				return 2
			}
		}
		if payload != nil {
			reportedHash, _ := payload["genesis_hash"].(string)
			if *expectGenesisHash != "" {
				if reportedHash != *expectGenesisHash {
					fmt.Fprintf(stderr,
						"[run_node] genesis hash mismatch: seed reports %s, expected %s\n",
						reportedHash, *expectGenesisHash)
					return 2
				}
				fmt.Fprintf(stdout, "[run_node] genesis hash matches the pinned %s\n", *expectGenesisHash)
			} else if allocation == "" {
				fmt.Fprintln(stdout,
					"[run_node] WARNING: joining on trust-on-first-use; pin the "+
						"seed with --expect-genesis-hash for a verified join")
			}
			if allocation == "" {
				allocation = applyGenesisParameters(payload)
			}
		}
	} else {
		allocation = identity.PublicKeyHex() + ":" + *genesisAmount
		fmt.Fprintf(stdout, "[run_node] founder mode: premine %s SAN to the node identity\n", *genesisAmount)
	}

	// A canonical genesis file is authoritative for the chain id; do not let
	// the flag default shadow it (ApplyGenesisFile would reject the conflict).
	if strings.TrimSpace(*genesisFile) == "" {
		envDefault("SAN_CHAIN_ID", *chainID)
	}
	envDefault("SAN_HOST", *host)
	envDefault("SAN_ADVERTISE_HOST", *host)
	envDefault("SAN_API_PORT", strconv.Itoa(apiPort))
	envDefault("SAN_P2P_PORT", strconv.Itoa(apiPort+700))
	envDefault("SAN_PEER_PORT", strconv.Itoa(apiPort+710))
	envDefault("SAN_CONTROLLER_PORT", strconv.Itoa(apiPort+720))
	envDefault("SAN_DB_PATH", dbPath)
	envDefault("SAN_KEY_FILE", *key)
	envDefault("SAN_GENESIS_FILE", *genesisFile)
	envDefault("SAN_GENESIS_ALLOCATION", allocation)
	envDefault("SAN_REWARD_ADDRESS", normalizedAddress)
	envDefault("SAN_BLOCK_THRESHOLD_FEE", "0.0001")
	if *bootstrap == "" {
		envDefault("SAN_BLOCK_REWARD", strconv.FormatFloat(*blockReward, 'f', -1, 64))
		if *blockReward > 0 {
			envDefault("SAN_MIN_BLOCK_INTERVAL_MS", "1000")
		}
	}
	if *bootstrap != "" {
		envDefault("SAN_BOOTSTRAP", *bootstrap)
	}

	fmt.Fprintf(stdout,
		"[run_node] starting\n"+
			"  reward address : %s\n"+
			"  node identity  : %s\n"+
			"  REST API       : http://%s:%d\n"+
			"  P2P (gRPC)     : %s:%d, peer %d, controller %d\n"+
			"  database       : %s\n"+
			"  chain id       : %s\n"+
			"  block reward   : %s SAN\n",
		normalizedAddress,
		*key,
		*host, apiPort,
		*host, apiPort+700, apiPort+710, apiPort+720,
		dbPath,
		os.Getenv("SAN_CHAIN_ID"),
		envOr("SAN_BLOCK_REWARD", "0"),
	)

	pinnedGenesis := *expectGenesisHash
	if pinnedGenesis == "" && payload != nil {
		pinnedGenesis, _ = payload["genesis_hash"].(string)
	}
	go superviseNode(apiPort, *key, *stake, pinnedGenesis, *bootstrap, *expectGenesisHash != "", stdout, stderr)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "[run_node] cannot create the database directory: %v\n", err)
		return 1
	}

	config := netnode.NodeConfigFromEnv()
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		fmt.Fprintf(stderr, "[run_node] cannot start the node: %v\n", err)
		return 1
	}
	logGenesisBanner(stdout, node)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "[run_node] cannot start the node: %v\n", err)
		return 1
	}
	defer node.Stop()

	if err := api.Run(ctx, node, config); err != nil {
		fmt.Fprintf(stderr, "[run_node] api server failed: %v\n", err)
		return 1
	}
	return 0
}

// runServe mirrors run.py: start the REST node from SAN_* environment alone.
func runServe(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	config := netnode.NodeConfigFromEnv()
	node, err := netnode.NewNode(config, nil)
	if err != nil {
		fmt.Fprintf(stderr, "cannot start the node: %v\n", err)
		return 1
	}
	logGenesisBanner(stdout, node)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := node.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "cannot start the node: %v\n", err)
		return 1
	}
	defer node.Stop()
	if err := api.Run(ctx, node, config); err != nil {
		fmt.Fprintf(stderr, "api server failed: %v\n", err)
		return 1
	}
	return 0
}

// logGenesisBanner prints the chain identity before the node accepts any
// remote data, so an operator can compare it with the published devnet
// genesis values (chain id, block hash, full fingerprint).
func logGenesisBanner(stdout io.Writer, node *netnode.Node) {
	fmt.Fprintf(stdout,
		"[run_node] genesis\n"+
			"  chain id       : %s\n"+
			"  genesis hash   : %v\n"+
			"  fingerprint    : %s\n"+
			"  software       : %s (protocol %d)\n",
		node.ChainID(), node.GenesisHash(), node.GenesisFingerprint(),
		node.SoftwareVersion(), netnode.ProtocolVersion)
}

// ---------------------------------------------------------------------- #
// Helpers (scripts/run_node.py)
// ---------------------------------------------------------------------- #

func envDefault(name, value string) {
	if current, ok := os.LookupEnv(name); !ok || current == "" {
		_ = os.Setenv(name, value)
	}
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}

func envIntDefault(name string, fallback int) int {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func keyStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func checkGenesisEnvironment() []string {
	conflicting := []string{}
	for _, name := range genesisParameterEnv {
		if os.Getenv(name) != "" {
			conflicting = append(conflicting, name)
		}
	}
	return conflicting
}

// fetchGenesis mirrors scripts/run_node.py fetch_genesis.
func fetchGenesis(bootstrap string, timeout time.Duration, stdout io.Writer) map[string]any {
	url := bootstrap
	if !strings.Contains(bootstrap, "://") {
		url = "http://" + bootstrap
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(strings.TrimRight(url, "/") + "/genesis")
	if err != nil {
		fmt.Fprintf(stdout, "[run_node] could not fetch genesis from %s: %v\n", url, err)
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		fmt.Fprintf(stdout, "[run_node] could not fetch genesis from %s: status %d\n", url, response.StatusCode)
		return nil
	}
	decoded, err := canonical.Decode(readAll(response))
	if err != nil {
		fmt.Fprintf(stdout, "[run_node] could not fetch genesis from %s: %v\n", url, err)
		return nil
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		fmt.Fprintf(stdout, "[run_node] could not fetch genesis from %s: unexpected payload\n", url)
		return nil
	}
	allocation, _ := payload["genesis_allocation"].(map[string]any)
	if len(allocation) == 0 {
		fmt.Fprintln(stdout, "[run_node] seed reported an empty genesis allocation")
		return nil
	}
	fmt.Fprintf(stdout, "[run_node] fetched genesis from %s (chain_id=%v, %d allocation(s))\n",
		url, payload["chain_id"], len(allocation))
	return payload
}

func readAll(response *http.Response) []byte {
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil
	}
	return data
}

// applyGenesisParameters mirrors scripts/run_node.py apply_genesis_parameters.
func applyGenesisParameters(payload map[string]any) string {
	parameters, _ := payload["parameters"].(map[string]any)

	unitsToEnv := map[string]string{
		"block_reward":        "SAN_BLOCK_REWARD",
		"min_validator_stake": "SAN_MIN_VALIDATOR_STAKE",
	}
	for key, envName := range unitsToEnv {
		if raw, ok := parameters[key]; ok {
			envDefault(envName, ledger.UnitsToSAN(toInt64(raw)))
		}
	}
	intToEnv := map[string]string{
		"unbonding_period": "SAN_UNBONDING_PERIOD",
		"slash_bps":        "SAN_SLASH_BPS",
		"block_gas_limit":  "SAN_BLOCK_GAS_LIMIT",
	}
	for key, envName := range intToEnv {
		if raw, ok := parameters[key]; ok {
			envDefault(envName, strconv.FormatInt(toInt64(raw), 10))
		}
	}
	if raw, ok := parameters["proposer_timeout_ms"]; ok {
		text, _ := canonical.MarshalString(float64(toInt64(raw)) / 1000.0)
		envDefault("SAN_PROPOSER_TIMEOUT", text)
	}
	if raw, ok := parameters["min_block_interval_ms"]; ok {
		envDefault("SAN_MIN_BLOCK_INTERVAL_MS", strconv.FormatInt(toInt64(raw), 10))
	}
	if chainID, ok := payload["chain_id"]; ok && chainID != nil {
		envDefault("SAN_CHAIN_ID", fmt.Sprintf("%v", chainID))
	}

	allocations, _ := payload["genesis_allocation"].(map[string]any)
	addresses := make([]string, 0, len(allocations))
	for address := range allocations {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	parts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parts = append(parts, address+":"+ledger.UnitsToSAN(toInt64(allocations[address])))
	}
	return strings.Join(parts, ",")
}

// seedHeight mirrors scripts/run_node.py _seed_height.
func seedHeight(bootstrap string) (int64, bool) {
	if bootstrap == "" {
		return 0, false
	}
	url := bootstrap
	if !strings.Contains(bootstrap, "://") {
		url = "http://" + bootstrap
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(strings.TrimRight(url, "/") + "/health")
	if err != nil {
		return 0, false
	}
	defer response.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, false
	}
	value, ok := payload["height"]
	if !ok {
		return 0, false
	}
	return toInt64(value), true
}

// superviseNode mirrors scripts/run_node.py supervise_node.
func superviseNode(apiPort int, keyFile string, amount float64, expectedGenesis, bootstrap string,
	genesisPinned bool, stdout, stderr io.Writer) {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	client := sdk.NewSanClient(baseURL, nil, 10*time.Second)
	identity, err := ledger.IdentityFromFile(keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "[run_node] cannot load identity %s: %v\n", keyFile, err)
		return
	}

	healthy := false
	for attempt := 0; attempt < 240; attempt++ {
		if _, err := client.Health(); err == nil {
			healthy = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !healthy {
		fmt.Fprintln(stderr, "[run_node] node never became healthy; check the log")
		return
	}

	if expectedGenesis != "" {
		local, err := sdk.NewSanClient(baseURL, nil, 10*time.Second).Genesis()
		if err != nil {
			fmt.Fprintf(stdout, "[run_node] could not read the local genesis: %v\n", err)
			os.Exit(1)
		}
		localHash, _ := local["genesis_hash"].(string)
		if localHash != expectedGenesis {
			fmt.Fprintf(stderr,
				"[run_node] FATAL: local genesis hash %s does not match the expected "+
					"%s; refusing to run on the wrong chain\n", localHash, expectedGenesis)
			os.Exit(1)
		}
		source := "the seed's reported hash"
		if genesisPinned {
			source = "the pinned hash"
		}
		fmt.Fprintf(stdout, "[run_node] local genesis verified against %s\n", source)
	}

	if bootstrap == "" {
		health, _ := client.Health()
		fmt.Fprintf(stdout, "[run_node] node is up and producing: height=%v finalized=%v\n",
			health["height"], health["finalized_height"])
	}
	peerHeight, hasPeerHeight := seedHeight(bootstrap)
	lastHeight := int64(-1)
	stable := 0
	iterations := 0
	if bootstrap != "" {
		iterations = 300
	}
	for attempt := 0; attempt < iterations; attempt++ {
		health, err := client.Health()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		height := toInt64(health["height"])
		peers := toInt64(health["peers"])
		if height != lastHeight {
			target := ""
			if hasPeerHeight {
				target = fmt.Sprintf(" (best peer: %d)", peerHeight)
			}
			fmt.Fprintf(stdout, "[run_node] syncing... height=%d%s peers=%d\n", height, target, peers)
			stable = 0
		} else {
			stable++
		}
		lastHeight = height
		if hasPeerHeight && height >= peerHeight {
			break
		}
		if !hasPeerHeight && stable >= 4 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if bootstrap != "" {
		finished, _ := client.Health()
		fmt.Fprintf(stdout, "[run_node] node is up and synced: height=%v peers=%v finalized=%v\n",
			finished["height"], finished["peers"], finished["finalized_height"])
	}

	if amount <= 0 {
		return
	}

	signingClient := sdk.NewSanClient(baseURL, identity, 10*time.Second)
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		fmt.Fprintf(stdout, "[run_node] staking failed: %v\n", err)
		return
	}
	account, err := signingClient.Account(address)
	if err != nil {
		fmt.Fprintf(stdout, "[run_node] staking failed: %v\n", err)
		return
	}
	balanceSAN, _ := strconv.ParseFloat(fmt.Sprintf("%v", account["balance"]), 64)
	active := map[string]bool{}
	if validators, err := signingClient.Validators(); err == nil {
		if records, ok := validators["validators"].([]any); ok {
			for _, raw := range records {
				if record, ok := raw.(map[string]any); ok {
					active[fmt.Sprintf("%v", record["address"])] = true
				}
			}
		}
	}
	if active[address] {
		fmt.Fprintln(stdout, "[run_node] validator already active; stake skipped")
		return
	}
	if balanceSAN < amount {
		fmt.Fprintf(stdout,
			"[run_node] balance %.6f SAN is below the stake (%v SAN); ask a funded account "+
				"to send you coins, then run: sancli --key %s stake --amount %v\n",
			balanceSAN, amount, keyFile, amount)
		return
	}
	result, err := signingClient.DepositStake(amount, nil)
	if err != nil {
		fmt.Fprintf(stdout, "[run_node] staking failed: %v\n", err)
		return
	}
	fmt.Fprintf(stdout, "[run_node] validator stake submitted: %s\n", sdk.PythonRepr(result))
}

func toInt64(value any) int64 {
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
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return parsed
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
