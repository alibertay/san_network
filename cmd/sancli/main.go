// Command sancli is the Go port of sdk/cli.py: a command line wallet and
// explorer for SAN Network.
//
// Examples:
//
//	sancli --rpc http://127.0.0.1:8000 health
//	sancli balance --address 0x...
//	sancli --key san_key.json send --to 0x... --value 5
//	sancli --key san_key.json deploy --id kv --file kv.pena
//	sancli query --id kv --function get
//	sancli metrics
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

type globalOptions struct {
	rpc     string
	key     string
	timeout float64
}

type stringList []string

func (list *stringList) String() string { return strings.Join(*list, ",") }

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

const usageText = `usage: sancli [-h] [--rpc RPC] [--key KEY] [--timeout TIMEOUT]
              {health,validators,finality,metrics,balance,send,stake,undelegate,withdraw,deploy,call,query,gov,proof,receipt,tx}
              ...

SAN Network wallet

positional arguments:
  {health,validators,finality,metrics,balance,send,stake,undelegate,withdraw,deploy,call,query,gov,proof,receipt,tx}

options:
  -h, --help         show this help message and exit
  --rpc RPC          node REST URL (default: http://127.0.0.1:8000)
  --key KEY          key file used to sign transactions
  --timeout TIMEOUT  request timeout in seconds (default: 10.0)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point (mirrors sdk.cli.main).
func run(argv []string, stdout, stderr io.Writer) int {
	options, rest, exit := parseGlobal(argv, stdout, stderr)
	if exit >= 0 {
		return exit
	}

	if len(rest) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	command, commandArgs := rest[0], rest[1:]

	var identity *ledger.NodeIdentity
	if options.key != "" {
		loaded, err := ledger.IdentityFromFile(options.key)
		if err != nil {
			fmt.Fprintf(stderr, "key error: %v\n", err)
			return 1
		}
		identity = loaded
	}

	client := sdk.NewSanClient(options.rpc, identity, time.Duration(options.timeout*float64(time.Second)))

	signCommands := map[string]bool{
		"send": true, "stake": true, "undelegate": true, "withdraw": true,
		"deploy": true, "call": true, "gov": true,
	}
	if signCommands[command] && identity == nil {
		fmt.Fprintln(stderr, "--key is required for transaction commands")
		return 1
	}

	var output any
	switch command {
	case "health":
		value, err := client.Health()
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "validators":
		value, err := client.Validators()
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "finality":
		value, err := client.Finality()
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "metrics":
		text, err := client.MetricsText()
		if err != nil {
			return reportError(stderr, err)
		}
		fmt.Fprint(stdout, text)
		return 0
	case "balance":
		args, code := parseBalance(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		address := args.address
		if address == "" {
			resolved, err := client.Address()
			if err != nil {
				return reportError(stderr, err)
			}
			address = resolved
		}
		value, err := client.Account(address)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "send":
		args, code := parseSend(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.Transfer(args.to, args.value, args.nonce)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "stake":
		args, code := parseStake(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.DepositStake(args.amount, nil)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "undelegate":
		if code := parseNoFlags(commandArgs, "undelegate", stderr); code >= 0 {
			return code
		}
		value, err := client.Undelegate(nil)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "withdraw":
		if code := parseNoFlags(commandArgs, "withdraw", stderr); code >= 0 {
			return code
		}
		value, err := client.WithdrawStake(nil)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "deploy":
		args, code := parseDeploy(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		source, err := os.ReadFile(args.file)
		if err != nil {
			return reportError(stderr, err)
		}
		value, err := client.DeployContract(args.id, string(source), args.gas, nil, nil)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "call":
		args, code := parseCall(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.CallContract(args.id, args.function, coerceParams(args.params), args.gas, nil, nil)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "query":
		args, code := parseQuery(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.ContractQuery(args.id, args.function, coerceParams(args.params))
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "gov":
		args, code := parseGov(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		nonce, err := client.Nonce("")
		if err != nil {
			return reportError(stderr, err)
		}
		approval, err := client.GovernanceApproval(args.name, args.value, &nonce)
		if err != nil {
			return reportError(stderr, err)
		}
		value, err := client.GovernanceSetParam(args.name, args.value, []any{approval}, &nonce)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "proof":
		args, code := parseProof(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.AccountProof(args.address)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "receipt":
		args, code := parseReceipt(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.Receipt(args.block, args.tx)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	case "tx":
		args, code := parseTx(commandArgs, stderr)
		if code >= 0 {
			return code
		}
		value, err := client.Transaction(args.id)
		if err != nil {
			return reportError(stderr, err)
		}
		output = value
	default:
		fmt.Fprintf(stderr, "sancli: unknown command: %s\n", command)
		return 2
	}

	if output != nil {
		if err := printJSON(stdout, output); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

// parseGlobal mirrors argparse's global options: they must precede the
// command. It returns (options, remaining args, exit code); exit < 0 means
// parsing succeeded.
func parseGlobal(argv []string, stdout, stderr io.Writer) (globalOptions, []string, int) {
	options := globalOptions{rpc: "http://127.0.0.1:8000", timeout: 10.0}
	index := 0
	for index < len(argv) {
		arg := argv[index]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprint(stdout, usageText)
			return options, nil, 0
		case arg == "--rpc":
			if index+1 >= len(argv) {
				return options, nil, globalError(stderr, "argument --rpc: expected one argument")
			}
			options.rpc = argv[index+1]
			index += 2
		case strings.HasPrefix(arg, "--rpc="):
			options.rpc = strings.TrimPrefix(arg, "--rpc=")
			index++
		case arg == "--key":
			if index+1 >= len(argv) {
				return options, nil, globalError(stderr, "argument --key: expected one argument")
			}
			options.key = argv[index+1]
			index += 2
		case strings.HasPrefix(arg, "--key="):
			options.key = strings.TrimPrefix(arg, "--key=")
			index++
		case arg == "--timeout":
			if index+1 >= len(argv) {
				return options, nil, globalError(stderr, "argument --timeout: expected one argument")
			}
			value, err := strconv.ParseFloat(argv[index+1], 64)
			if err != nil {
				return options, nil, globalError(stderr, "argument --timeout: invalid float value: "+argv[index+1])
			}
			options.timeout = value
			index += 2
		case strings.HasPrefix(arg, "--timeout="):
			value, err := strconv.ParseFloat(strings.TrimPrefix(arg, "--timeout="), 64)
			if err != nil {
				return options, nil, globalError(stderr, "argument --timeout: invalid float value")
			}
			options.timeout = value
			index++
		case strings.HasPrefix(arg, "-"):
			return options, nil, globalError(stderr, "unrecognized arguments: "+arg)
		default:
			return options, argv[index:], -1
		}
	}
	return options, nil, -1
}

func globalError(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "usage: sancli [-h] [--rpc RPC] [--key KEY] [--timeout TIMEOUT] ...\n")
	fmt.Fprintf(stderr, "sancli: error: %s\n", message)
	return 2
}

func reportError(stderr io.Writer, err error) int {
	var clientErr *sdk.SanClientError
	message := err.Error()
	if errors.As(err, &clientErr) {
		message = clientErr.Message
	}
	payload := map[string]string{"error": message}
	encoder := json.NewEncoder(stderr)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
	return 1
}

func printJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// coerceParams mirrors sdk.cli._coerce: int when possible, string otherwise.
func coerceParams(params []string) []any {
	coerced := make([]any, 0, len(params))
	for _, param := range params {
		if integer, err := strconv.ParseInt(param, 10, 64); err == nil {
			coerced = append(coerced, integer)
		} else {
			coerced = append(coerced, param)
		}
	}
	return coerced
}

// ---------------------------------------------------------------------- #
// Subcommand flag parsing (mirrors sdk/cli.py build_parser)
// ---------------------------------------------------------------------- #

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	set.Usage = func() {
		fmt.Fprintf(stderr, "usage: sancli %s [options]\n", name)
	}
	return set
}

func flagError(stderr io.Writer, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

type balanceOptions struct{ address string }

func parseBalance(argv []string, stderr io.Writer) (balanceOptions, int) {
	set := newFlagSet("balance", stderr)
	address := set.String("address", "", "")
	if err := set.Parse(argv); err != nil {
		return balanceOptions{}, flagError(stderr, err)
	}
	return balanceOptions{address: *address}, -1
}

type sendOptions struct {
	to    string
	value float64
	nonce *int64
}

func parseSend(argv []string, stderr io.Writer) (sendOptions, int) {
	set := newFlagSet("send", stderr)
	to := set.String("to", "", "")
	value := set.String("value", "", "")
	nonce := set.Int64("nonce", 0, "")
	if err := set.Parse(argv); err != nil {
		return sendOptions{}, flagError(stderr, err)
	}
	if *to == "" || *value == "" {
		fmt.Fprintln(stderr, "sancli send: error: --to and --value are required")
		return sendOptions{}, 2
	}
	parsedValue, err := strconv.ParseFloat(*value, 64)
	if err != nil {
		fmt.Fprintf(stderr, "sancli send: error: invalid --value: %s\n", *value)
		return sendOptions{}, 2
	}
	result := sendOptions{to: *to, value: parsedValue}
	set.Visit(func(f *flag.Flag) {
		if f.Name == "nonce" {
			current := *nonce
			result.nonce = &current
		}
	})
	return result, -1
}

type stakeOptions struct{ amount float64 }

func parseStake(argv []string, stderr io.Writer) (stakeOptions, int) {
	set := newFlagSet("stake", stderr)
	amount := set.String("amount", "", "")
	if err := set.Parse(argv); err != nil {
		return stakeOptions{}, flagError(stderr, err)
	}
	if *amount == "" {
		fmt.Fprintln(stderr, "sancli stake: error: --amount is required")
		return stakeOptions{}, 2
	}
	parsed, err := strconv.ParseFloat(*amount, 64)
	if err != nil {
		fmt.Fprintf(stderr, "sancli stake: error: invalid --amount: %s\n", *amount)
		return stakeOptions{}, 2
	}
	return stakeOptions{amount: parsed}, -1
}

func parseNoFlags(argv []string, name string, stderr io.Writer) int {
	set := newFlagSet(name, stderr)
	if err := set.Parse(argv); err != nil {
		return flagError(stderr, err)
	}
	if set.NArg() > 0 {
		fmt.Fprintf(stderr, "sancli: error: unrecognized arguments: %s\n", strings.Join(set.Args(), " "))
		return 2
	}
	return -1
}

type deployOptions struct {
	id   string
	file string
	gas  int64
}

func parseDeploy(argv []string, stderr io.Writer) (deployOptions, int) {
	set := newFlagSet("deploy", stderr)
	id := set.String("id", "", "")
	file := set.String("file", "", "")
	gas := set.Int64("gas", 2_000_000, "")
	if err := set.Parse(argv); err != nil {
		return deployOptions{}, flagError(stderr, err)
	}
	if *id == "" || *file == "" {
		fmt.Fprintln(stderr, "sancli deploy: error: --id and --file are required")
		return deployOptions{}, 2
	}
	return deployOptions{id: *id, file: *file, gas: *gas}, -1
}

type callOptions struct {
	id       string
	function string
	params   []string
	gas      int64
}

func parseCall(argv []string, stderr io.Writer) (callOptions, int) {
	set := newFlagSet("call", stderr)
	id := set.String("id", "", "")
	function := set.String("function", "", "")
	gas := set.Int64("gas", 1_000_000, "")
	var params stringList
	set.Var(&params, "param", "")
	if err := set.Parse(argv); err != nil {
		return callOptions{}, flagError(stderr, err)
	}
	if *id == "" || *function == "" {
		fmt.Fprintln(stderr, "sancli call: error: --id and --function are required")
		return callOptions{}, 2
	}
	return callOptions{id: *id, function: *function, params: params, gas: *gas}, -1
}

type queryOptions struct {
	id       string
	function string
	params   []string
}

func parseQuery(argv []string, stderr io.Writer) (queryOptions, int) {
	set := newFlagSet("query", stderr)
	id := set.String("id", "", "")
	function := set.String("function", "", "")
	var params stringList
	set.Var(&params, "param", "")
	if err := set.Parse(argv); err != nil {
		return queryOptions{}, flagError(stderr, err)
	}
	if *id == "" || *function == "" {
		fmt.Fprintln(stderr, "sancli query: error: --id and --function are required")
		return queryOptions{}, 2
	}
	return queryOptions{id: *id, function: *function, params: params}, -1
}

type govOptions struct {
	name  string
	value int64
}

func parseGov(argv []string, stderr io.Writer) (govOptions, int) {
	set := newFlagSet("gov", stderr)
	name := set.String("name", "", "")
	value := set.Int64("value", 0, "")
	if err := set.Parse(argv); err != nil {
		return govOptions{}, flagError(stderr, err)
	}
	provided := map[string]bool{}
	set.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if *name == "" || !provided["value"] {
		fmt.Fprintln(stderr, "sancli gov: error: --name and --value are required")
		return govOptions{}, 2
	}
	return govOptions{name: *name, value: *value}, -1
}

type proofOptions struct{ address string }

func parseProof(argv []string, stderr io.Writer) (proofOptions, int) {
	set := newFlagSet("proof", stderr)
	address := set.String("address", "", "")
	if err := set.Parse(argv); err != nil {
		return proofOptions{}, flagError(stderr, err)
	}
	return proofOptions{address: *address}, -1
}

type receiptOptions struct {
	block int64
	tx    int64
}

func parseReceipt(argv []string, stderr io.Writer) (receiptOptions, int) {
	set := newFlagSet("receipt", stderr)
	block := set.Int64("block", 0, "")
	tx := set.Int64("tx", 0, "")
	if err := set.Parse(argv); err != nil {
		return receiptOptions{}, flagError(stderr, err)
	}
	provided := map[string]bool{}
	set.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if !provided["block"] || !provided["tx"] {
		fmt.Fprintln(stderr, "sancli receipt: error: --block and --tx are required")
		return receiptOptions{}, 2
	}
	return receiptOptions{block: *block, tx: *tx}, -1
}

type txOptions struct{ id string }

func parseTx(argv []string, stderr io.Writer) (txOptions, int) {
	set := newFlagSet("tx", stderr)
	id := set.String("id", "", "")
	if err := set.Parse(argv); err != nil {
		return txOptions{}, flagError(stderr, err)
	}
	if *id == "" {
		fmt.Fprintln(stderr, "sancli tx: error: --id is required")
		return txOptions{}, 2
	}
	return txOptions{id: *id}, -1
}
