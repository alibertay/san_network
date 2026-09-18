package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

const faucetUsageText = `usage: sanup faucet --to ADDRESS [options]

Ask a running node's faucet (SAN_FAUCET=1) to fund an address.

options:
  --to ADDRESS        receiver address (0x..., required)
  --amount SAN        amount in SAN (default: the node's faucet amount)
  --rpc URL           node REST endpoint (default http://127.0.0.1:8000)
  --api-token TOKEN   sent as Authorization: Bearer TOKEN
  --tls-ca FILE       CA bundle used to verify the node certificate

example:
  sanup faucet --to 0xab... --amount 10 --rpc http://127.0.0.1:8000
`

func runFaucet(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sanup faucet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, faucetUsageText) }
	to := flags.String("to", "", "")
	amount := flags.Float64("amount", -1, "")
	rpc := flags.String("rpc", "http://127.0.0.1:8000", "")
	apiToken := flags.String("api-token", envDefaultString("SAN_API_TOKEN", ""), "")
	tlsCA := flags.String("tls-ca", envDefaultString("SAN_TLS_CA", ""), "")
	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sanup faucet: error: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	address := strings.TrimSpace(*to)
	if address == "" {
		fmt.Fprintln(stderr, "sanup faucet: error: --to is required")
		return 2
	}
	if !ledger.IsValidAddress(address) {
		fmt.Fprintf(stderr, "sanup faucet: error: invalid address: %s\n", address)
		return 2
	}
	if *amount == 0 || *amount < -1 {
		fmt.Fprintln(stderr, "sanup faucet: error: --amount must be a positive SAN number (omit it for the node default)")
		return 2
	}
	var amountArg any
	if *amount > 0 {
		amountArg = *amount
	}

	baseURL := strings.TrimSpace(*rpc)
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	client := sdk.NewSanClient(baseURL, nil, 15*time.Second).SetToken(*apiToken)
	if strings.TrimSpace(*tlsCA) != "" {
		if err := client.SetTLS(*tlsCA, false); err != nil {
			fmt.Fprintf(stderr, "sanup faucet: error: %v\n", err)
			return 1
		}
	} else if strings.HasPrefix(baseURL, "https://") {
		// The devnet node uses a self-signed certificate; without --tls-ca
		// the CLI still talks to it (same as sanup itself).
		_ = client.SetTLS("", true)
	}

	result, err := client.Faucet(address, amountArg)
	if err != nil {
		fmt.Fprintf(stderr, "sanup faucet: error: %v\n", err)
		return 1
	}
	txID, _ := result["tx_id"].(string)
	amountText, _ := result["amount"].(string)
	status, _ := result["status"].(string)
	if amountText == "" {
		amountText = ledger.UnitsToSAN(0)
	}
	logf(stdout, "faucet sent %s SAN to %s (status %s, tx %s)", amountText, address, status, txID)
	return 0
}
