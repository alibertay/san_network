// Command sansoak is the SAN Network soak/chaos runner. It spawns (or
// targets) a local multi-node devnet, continuously generates activity
// (transfers, SANRC20, contract writes/reads, staking, validator churn,
// governance, restarts) and periodically asserts the public-devnet
// invariants, then prints a PASS/FAIL report and exits non-zero on failure.
//
//	go run ./cmd/sansoak --nodes 3 --duration 24h
//	go run ./cmd/sansoak --short                 # ~70s CI mode
//	go run ./cmd/sansoak --rpc http://127.0.0.1:8000,http://127.0.0.1:8001
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/devnet"
	"github.com/alibertay/san_network/internal/soak"
)

const usageText = `usage: sansoak [options]

Soak and chaos-verify a local SAN devnet (or a set of running nodes).

options:
  --nodes N          number of nodes to spawn (default 3, max 5)
  --duration DUR     soak duration (default 24h); accepts Go durations
  --short            CI mode: 70s, lower activity, no restarts
  --rpc LIST         target running nodes (comma-separated REST URLs);
                     skips spawning
  --data-dir DIR     parent directory for the spawned node data
  --seed N           activity RNG seed (deterministic activity order)
  --interval DUR     activity tick (default 1s short / 3s long)
  --settle DUR       quiet period before the final report (default 5s)
  --max-mempool N    mempool bound assertion (default 2048)
  --keep             keep the spawned node data for debugging
  --json             print the final report as JSON
  --report FILE      also write the JSON report to FILE

exit status: 0 when every invariant passes, 1 on any failure, 2 on bad flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sansoak", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usageText) }
	nodes := flags.Int("nodes", 3, "")
	duration := flags.Duration("duration", 24*time.Hour, "")
	short := flags.Bool("short", false, "")
	rpc := flags.String("rpc", "", "")
	dataDir := flags.String("data-dir", "", "")
	seed := flags.Int64("seed", 1, "")
	interval := flags.Duration("interval", 0, "")
	settle := flags.Duration("settle", 5*time.Second, "")
	maxMempool := flags.Int64("max-mempool", 2048, "")
	keep := flags.Bool("keep", false, "")
	jsonOut := flags.Bool("json", false, "")
	reportFile := flags.String("report", "", "")
	durationSet := false
	flags.Visit(func(defined *flag.Flag) {
		if defined.Name == "duration" {
			durationSet = true
		}
	})
	if err := flags.Parse(argv); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sansoak: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *short {
		if !durationSet {
			*duration = 70 * time.Second
		}
		if *interval == 0 {
			*interval = 700 * time.Millisecond
		}
	} else if *interval == 0 {
		*interval = 3 * time.Second
	}
	if *nodes < 1 || *nodes > 5 {
		fmt.Fprintln(stderr, "sansoak: --nodes must be between 1 and 5")
		return 2
	}

	runner := &runner{
		stdout:     stdout,
		stderr:     stderr,
		duration:   *duration,
		interval:   *interval,
		settle:     *settle,
		short:      *short,
		maxMempool: *maxMempool,
		keep:       *keep,
		jsonOut:    *jsonOut,
		reportFile: *reportFile,
		seed:       *seed,
	}
	var code int
	if strings.TrimSpace(*rpc) != "" {
		code = runner.runRPC(*rpc, *dataDir)
	} else {
		code = runner.runSpawn(*nodes, *dataDir)
	}
	return code
}

// runner holds one soak execution.
type runner struct {
	stdout     io.Writer
	stderr     io.Writer
	duration   time.Duration
	interval   time.Duration
	settle     time.Duration
	short      bool
	maxMempool int64
	keep       bool
	jsonOut    bool
	reportFile string
	seed       int64
}

// runSpawn starts a fresh local devnet and soaks it.
func (r *runner) runSpawn(nodes int, dataDir string) int {
	started := time.Now()
	tempBase := dataDir
	if tempBase == "" {
		tempBase = filepath.Join("data", "soak")
		_ = os.MkdirAll(tempBase, 0o755)
	}
	harness, err := devnet.New("", tempBase, func(format string, args ...any) {
		fmt.Fprintf(r.stdout, "[soak] "+format+"\n", args...)
	})
	if err != nil {
		fmt.Fprintf(r.stderr, "sansoak: %v\n", err)
		return 2
	}
	harness.Keep = r.keep
	defer harness.Cleanup()
	if err := harness.AddNodes(nodes); err != nil {
		fmt.Fprintf(r.stderr, "sansoak: %v\n", err)
		return 2
	}
	state, err := newSoakState(harness, r)
	if err != nil {
		fmt.Fprintf(r.stderr, "sansoak: %v\n", err)
		return 1
	}
	defer state.close()
	if err := state.setup(); err != nil {
		fmt.Fprintf(r.stderr, "sansoak: setup failed: %v\n", err)
		return 1
	}
	r.logf("devnet ready in %.1fs (%d nodes)", time.Since(started).Seconds(), nodes)
	return r.soak(state, started)
}

// runRPC targets already running nodes.
func (r *runner) runRPC(list, dataDir string) int {
	started := time.Now()
	targets := []string{}
	for _, entry := range strings.Split(list, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(r.stderr, "sansoak: --rpc needs at least one URL")
		return 2
	}
	harness := &devnet.Harness{Logf: func(format string, args ...any) { r.logf(format, args...) }}
	for index, target := range targets {
		url := target
		if !strings.Contains(url, "://") {
			url = "http://" + url
		}
		harness.Nodes = append(harness.Nodes, &devnet.NodeSpec{
			Name: fmt.Sprintf("R%d", index),
			URL:  url,
		})
	}
	r.logf("targeting %d running node(s) in RPC mode (activity limited to reads)", len(targets))
	state, err := newSoakState(harness, r)
	if err != nil {
		fmt.Fprintf(r.stderr, "sansoak: %v\n", err)
		return 1
	}
	defer state.close()
	if err := state.setup(); err != nil {
		fmt.Fprintf(r.stderr, "sansoak: setup failed: %v\n", err)
		return 1
	}
	return r.soak(state, started)
}

func (r *runner) logf(format string, args ...any) {
	fmt.Fprintf(r.stdout, "[soak] "+format+"\n", args...)
}

// soak runs the activity/sampling loop and the final report.
func (r *runner) soak(state *soakState, started time.Time) int {
	deadline := time.Now().Add(r.duration)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	sampleEvery := int64(5)
	if r.interval < time.Second {
		sampleEvery = 8
	}
	r.logf("soaking for %s (interval %s, settle %s)", r.duration, r.interval, r.settle)
	cycles := int64(0)
	for time.Now().Before(deadline) {
		<-ticker.C
		cycles++
		state.cycles = cycles
		state.tick(cycles)
		if cycles%sampleEvery == 0 {
			state.sample(true)
			r.progress(state, started)
		}
	}

	r.logf("settling for %s before the final assertions", r.settle)
	time.Sleep(r.settle)
	state.sample(false)

	return r.finish(state, started)
}

func (r *runner) progress(state *soakState, started time.Time) {
	heights := make([]string, 0, len(state.nodes))
	for _, node := range state.nodes {
		view, ok := state.lastView[node.name]
		if !ok {
			heights = append(heights, node.name+"=?")
			continue
		}
		heights = append(heights, fmt.Sprintf("%s=%d/%d", node.name, view.Height, view.FinalizedHeight))
	}
	r.logf("t=%.0fs cycles=%d txs=%d activity_errors=%d heights=%s",
		time.Since(started).Seconds(), state.cycles, state.transactions,
		state.activityErrors, strings.Join(heights, " "))
}

func (r *runner) finish(state *soakState, started time.Time) int {
	views := state.finalViews()
	checks := soak.Evaluate(views, soak.Options{
		HeightTolerance:    3,
		FinalityTolerance:  12,
		MaxMempool:         r.maxMempool,
		MaxGoroutineGrowth: 4.0,
		MaxMemoryGrowth:    6.0,
		MinHeightProgress:  1,
	})
	checks = append(checks, soak.CheckGrowth(state.growthPoints()))
	checks = append(checks, state.activityCheck())

	report := soak.Build(time.Since(started), len(state.nodes), state.cycles, state.transactions, checks)

	rendered := ""
	if encoded, err := report.JSON(); err == nil {
		rendered = string(encoded)
		if r.reportFile != "" {
			if err := os.WriteFile(r.reportFile, encoded, 0o644); err != nil {
				fmt.Fprintf(r.stderr, "sansoak: cannot write %s: %v\n", r.reportFile, err)
			}
		}
	}
	if r.jsonOut {
		fmt.Fprintln(r.stdout, rendered)
	} else {
		report.Render(r.stdout)
	}
	if !report.Pass() {
		return 1
	}
	return 0
}

// parseIntValue parses the string/number shapes the REST API emits.
func parseIntValue(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed
	default:
		return devnet.ToInt64(value)
	}
}
