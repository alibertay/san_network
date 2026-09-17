// Command sangenesis is the Go port of scripts/genesis_bootstrap.py: it
// generates the node identities for a brand-new SAN devnet first, prints the
// shared SAN_GENESIS_ALLOCATION string and the commands to start each node.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alibertay/san_network/internal/ledger"
)

const usage = `usage: sangenesis [options]

Genesis bootstrap for a brand-new SAN devnet.

options:
  --count N            number of nodes (default 1)
  --dir DIR            key output directory (default devnet-keys)
  --chain-id ID        chain id (default san-devnet-1)
  --amount AMOUNT      SAN premined to every node (default 1000000)
  --api-port PORT      first API port (default 8000)
  --write-env          write devnet.env files
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr *os.File) int {
	flags := flag.NewFlagSet("sangenesis", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	count := flags.Int("count", 1, "")
	dir := flags.String("dir", "devnet-keys", "")
	chainID := flags.String("chain-id", "san-devnet-1", "")
	amount := flags.String("amount", "1000000", "")
	apiPort := flags.Int("api-port", 8000, "")
	writeEnv := flags.Bool("write-env", false, "")
	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *count < 1 {
		fmt.Fprintln(stderr, "--count must be at least 1")
		return 2
	}

	keyDir := *dir
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "cannot create %s: %v\n", keyDir, err)
		return 1
	}

	identities := make([]*ledger.NodeIdentity, 0, *count)
	for index := 1; index <= *count; index++ {
		keyFile := filepath.Join(keyDir, fmt.Sprintf("key%d.json", index))
		identity, err := ledger.LoadIdentity(keyFile, true)
		if err != nil {
			fmt.Fprintf(stderr, "cannot load identity %s: %v\n", keyFile, err)
			return 1
		}
		identities = append(identities, identity)
		address, err := ledger.AddressFromPublicKey(identity.PublicKey)
		if err != nil {
			fmt.Fprintf(stderr, "cannot derive address: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "node %d: key=%s  address=%s\n", index, keyFile, address)
		fmt.Fprintf(stdout, "          pubkey=%s\n", identity.PublicKeyHex())
	}

	allocationParts := make([]string, 0, len(identities))
	for _, identity := range identities {
		allocationParts = append(allocationParts, identity.PublicKeyHex()+":"+*amount)
	}
	allocation := strings.Join(allocationParts, ",")
	seedPort := *apiPort

	fmt.Fprintln(stdout, "\n# 1) Everyone must share this exact string (it defines genesis):")
	fmt.Fprintf(stdout, "export SAN_GENESIS_ALLOCATION=\"%s\"\n", allocation)
	fmt.Fprintf(stdout, "export SAN_CHAIN_ID=\"%s\"\n", *chainID)

	fmt.Fprintln(stdout, "\n# 2) Start the seed node (node 1):")
	fmt.Fprintf(stdout,
		"SAN_API_PORT=%d SAN_P2P_PORT=%d SAN_PEER_PORT=%d SAN_CONTROLLER_PORT=%d \\\n"+
			"SAN_CHAIN_ID=\"%s\" SAN_DB_PATH=\"%s/node1.kv\" SAN_KEY_FILE=\"%s/key1.json\" \\\n"+
			"SAN_GENESIS_ALLOCATION=\"%s\" SAN_BLOCK_THRESHOLD_FEE=0.0001 \\\n"+
			"sannode serve\n",
		seedPort, seedPort+700, seedPort+710, seedPort+720,
		*chainID,
		keyDir,
		keyDir,
		allocation,
	)

	for index := 2; index <= *count; index++ {
		api := seedPort + index - 1
		fmt.Fprintf(stdout, "\n# 3.%d) Start node %d (bootstrap to node 1):\n", index-1, index)
		fmt.Fprintf(stdout,
			"SAN_API_PORT=%d SAN_P2P_PORT=%d SAN_PEER_PORT=%d SAN_CONTROLLER_PORT=%d \\\n"+
				"SAN_CHAIN_ID=\"%s\" SAN_DB_PATH=\"%s/node%d.kv\" SAN_KEY_FILE=\"%s/key%d.json\" \\\n"+
				"SAN_GENESIS_ALLOCATION=\"%s\" SAN_BLOCK_THRESHOLD_FEE=0.0001 \\\n"+
				"SAN_BOOTSTRAP=127.0.0.1:%d \\\n"+
				"sannode serve\n",
			api, api+700, api+710, api+720,
			*chainID,
			keyDir, index,
			keyDir, index,
			allocation,
			seedPort,
		)
	}

	fmt.Fprintf(stdout,
		"\n# 4) Activate validators (each node, from its own key directory):\n"+
			"#    sancli --rpc http://127.0.0.1:%d --key %s/key1.json stake --amount 1000\n"+
			"#    Once >=1 validator is active, /finality starts advancing.\n",
		seedPort,
		keyDir,
	)

	if *writeEnv {
		for index := 1; index <= len(identities); index++ {
			api := seedPort + index - 1
			envFile := filepath.Join(keyDir, fmt.Sprintf("node%d.env", index))
			lines := []string{
				"SAN_CHAIN_ID=" + *chainID,
				"SAN_HOST=127.0.0.1",
				"SAN_ADVERTISE_HOST=127.0.0.1",
				fmt.Sprintf("SAN_API_PORT=%d", api),
				fmt.Sprintf("SAN_P2P_PORT=%d", api+700),
				fmt.Sprintf("SAN_PEER_PORT=%d", api+710),
				fmt.Sprintf("SAN_CONTROLLER_PORT=%d", api+720),
				fmt.Sprintf("SAN_DB_PATH=%s/node%d.kv", keyDir, index),
				fmt.Sprintf("SAN_KEY_FILE=%s/key%d.json", keyDir, index),
				fmt.Sprintf("SAN_GENESIS_ALLOCATION=\"%s\"", allocation),
				"SAN_BLOCK_THRESHOLD_FEE=0.0001",
			}
			if index > 1 {
				lines = append(lines, fmt.Sprintf("SAN_BOOTSTRAP=127.0.0.1:%d", seedPort))
			}
			// Python's Path.write_text translates \n to os.linesep; match it.
			content := strings.Join(lines, "\n") + "\n"
			if runtime.GOOS == "windows" {
				content = strings.ReplaceAll(content, "\n", "\r\n")
			}
			if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
				fmt.Fprintf(stderr, "cannot write %s: %v\n", envFile, err)
				return 1
			}
			fmt.Fprintf(stdout, "wrote %s\n", envFile)
		}
	}
	return 0
}
