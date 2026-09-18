// Wallet key management for sancli: create key files and derive addresses.
//
// Examples:
//
//	sancli wallet new --out devnet/wallet.json
//	sancli wallet address --key devnet/wallet.json
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/alibertay/san_network/internal/ledger"
)

const walletUsage = `usage: sancli wallet {new,address} [options]

create or inspect a wallet key file (same JSON format as the Python SDK).

subcommands:
  new [--out FILE] [--force]   create a key file (default wallet.json)
  address --key FILE           print the address of an existing key file

options:
  -h, --help                   show this help message
`

func runWallet(argv []string, globalKey string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprint(stderr, walletUsage)
		return 2
	}
	switch argv[0] {
	case "new":
		return runWalletNew(argv[1:], stdout, stderr)
	case "address":
		return runWalletAddress(argv[1:], globalKey, stdout, stderr)
	case "-h", "--help":
		fmt.Fprint(stdout, walletUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "sancli wallet: unknown subcommand: %s\n", argv[0])
		fmt.Fprint(stderr, walletUsage)
		return 2
	}
}

func runWalletNew(argv []string, stdout, stderr io.Writer) int {
	set := newFlagSet("wallet new", stderr)
	out := set.String("out", "wallet.json", "")
	force := set.Bool("force", false, "")
	if err := set.Parse(argv); err != nil {
		return flagError(stderr, err)
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		fmt.Fprintf(stderr, "sancli wallet new: %s already exists (use --force to overwrite)\n", *out)
		return 1
	}
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		fmt.Fprintf(stderr, "sancli wallet new: %v\n", err)
		return 1
	}
	if err := identity.Save(*out); err != nil {
		fmt.Fprintf(stderr, "sancli wallet new: cannot write %s: %v\n", *out, err)
		return 1
	}
	return writeWalletInfo(stdout, stderr, identity, *out)
}

func runWalletAddress(argv []string, globalKey string, stdout, stderr io.Writer) int {
	set := newFlagSet("wallet address", stderr)
	key := set.String("key", globalKey, "")
	if err := set.Parse(argv); err != nil {
		return flagError(stderr, err)
	}
	if *key == "" {
		fmt.Fprintln(stderr, "sancli wallet address: error: --key is required")
		return 2
	}
	identity, err := ledger.IdentityFromFile(*key)
	if err != nil {
		fmt.Fprintf(stderr, "sancli wallet address: %v\n", err)
		return 1
	}
	return writeWalletInfo(stdout, stderr, identity, *key)
}

func writeWalletInfo(stdout, stderr io.Writer, identity *ledger.NodeIdentity, keyFile string) int {
	address, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		fmt.Fprintf(stderr, "sancli wallet: cannot derive address: %v\n", err)
		return 1
	}
	payload := map[string]any{
		"address":    address,
		"public_key": identity.PublicKeyHex(),
		"key_file":   keyFile,
	}
	if err := printJSON(stdout, payload); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
