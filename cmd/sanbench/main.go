// Command sanbench measures the performance characteristics of the SAN
// Network Go implementation and emits machine-readable JSON plus a human
// table. Methodology is documented in docs/benchmarks.md; never quote raw
// numbers without it.
//
//	go run ./cmd/sanbench            # fast default mode
//	go run ./cmd/sanbench --full     # larger workloads + local network lab
//	go run ./cmd/sanbench --json     # machine-readable output
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alibertay/san_network/internal/bench"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/version"
)

const usageText = `usage: sanbench [options]

Benchmark the SAN node: ML-DSA-44, transactions, SANVM, block validation and
(optionally) a local network lab. Output is a human table by default and JSON
with --json.

options:
  --iterations N     crypto/tx iteration count (default 200)
  --vm-iterations N  SANVM call iterations (default 200)
  --tx-count N       transactions per serialization/validation sample (default 500)
  --full             larger workloads (5000-tx block) plus the network lab
  --network          run only the local propagation/sync lab
  --json             print JSON instead of the table
  --out FILE         also write the JSON report to FILE
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sanbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usageText) }
	iterations := flags.Int("iterations", 200, "")
	vmIterations := flags.Int("vm-iterations", 200, "")
	txCount := flags.Int("tx-count", 500, "")
	full := flags.Bool("full", false, "")
	network := flags.Bool("network", false, "")
	jsonOut := flags.Bool("json", false, "")
	outFile := flags.String("out", "", "")
	if err := flags.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *full {
		if *iterations == 200 {
			*iterations = 1000
		}
		if *vmIterations == 200 {
			*vmIterations = 1000
		}
		if *txCount == 500 {
			*txCount = 2000
		}
	}

	started := time.Now()
	results := []bench.Result{}
	mode := "default"
	if *full {
		mode = "full"
	}
	runAll := !*network
	if runAll {
		bench.SilenceOutput(func() {
			results = append(results, bench.Signatures(*iterations)...)
			results = append(results, bench.Sizes()...)
			results = append(results, bench.Transactions(*txCount)...)
			results = append(results, bench.VM(*vmIterations)...)
			blockSizes := []int{10, 100, 1000}
			if *full {
				blockSizes = append(blockSizes, 5000)
			}
			results = append(results, bench.BlockValidation(blockSizes)...)
		})
	}
	if *network || *full {
		networkResults, err := benchNetwork(stdout)
		if err != nil {
			results = append(results, bench.Result{Name: "network", Unit: "lab", Error: err.Error()})
		} else {
			results = append(results, networkResults...)
		}
	}

	report := map[string]any{
		"environment": bench.Describe(mode),
		"version":     version.Resolve(netnode.ProtocolVersion, ledger.SchemaVersion).Map(),
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"duration_s":  time.Since(started).Seconds(),
		"results":     results,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "sanbench: %v\n", err)
		return 1
	}
	if *outFile != "" {
		if err := os.WriteFile(*outFile, encoded, 0o644); err != nil {
			fmt.Fprintf(stderr, "sanbench: cannot write %s: %v\n", *outFile, err)
		}
	}
	if *jsonOut {
		fmt.Fprintln(stdout, string(encoded))
	} else {
		renderTable(stdout, runAll, results)
	}
	failures := 0
	for _, result := range results {
		if result.Error != "" {
			failures++
		}
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func renderTable(w io.Writer, runAll bool, results []bench.Result) {
	environment := bench.Describe("")
	fmt.Fprintf(w, "SAN benchmark (%s/%s, %d CPUs, %s, %s)\n",
		environment.OS, environment.Arch, environment.CPUs, environment.Go, environment.Backend)
	fmt.Fprintf(w, "%-28s %16s %-10s %s\n", "benchmark", "value", "unit", "details")
	for _, result := range results {
		if result.Error != "" {
			fmt.Fprintf(w, "%-28s %16s %-10s ERROR %s\n", result.Name, "-", result.Unit, result.Error)
			continue
		}
		details := ""
		for key, value := range result.Extra {
			if details != "" {
				details += " "
			}
			details += fmt.Sprintf("%s=%v", key, value)
		}
		fmt.Fprintf(w, "%-28s %16.2f %-10s %s\n", result.Name, result.Value, result.Unit, details)
	}
	fmt.Fprintln(w, "\nmethodology: docs/benchmarks.md (operations, hardware and caveats)")
}
