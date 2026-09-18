package main

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/alibertay/san_network/internal/bench"
	"github.com/alibertay/san_network/internal/devnet"
)

// benchNetwork runs a local two-node lab and measures block/tx propagation
// and restart sync throughput. It spawns real sanup nodes; --full enables it
// automatically.
func benchNetwork(stdout io.Writer) ([]bench.Result, error) {
	logf := func(format string, args ...any) {
		fmt.Fprintf(stdout, "[sanbench-lab] "+format+"\n", args...)
	}
	harness, err := devnet.New("", "", logf)
	if err != nil {
		return nil, err
	}
	harness.Keep = false
	// The lab polls /health at millisecond resolution to timestamp block
	// observations; raise the REST rate limit for the benchmark nodes only.
	harness.Env = append(harness.Env, "SAN_RPC_RATE_LIMIT=100000")
	defer harness.Cleanup()
	if err := harness.AddNodes(2); err != nil {
		return nil, err
	}
	seed, joiner := harness.Nodes[0], harness.Nodes[1]

	logf("starting the founder node")
	if err := harness.StartSeed(seed); err != nil {
		return nil, err
	}
	if err := harness.WaitHealthy(seed, 90*time.Second); err != nil {
		return nil, err
	}
	// Let the founder build a backlog first; the joiner then syncs it.
	if err := harness.WaitHeight(seed, 15, 120*time.Second); err != nil {
		return nil, err
	}
	// Stake the founder so block production has a deterministic proposer
	// while the joiner is measured.
	if err := harness.RunNode(seed, "--stake", "100"); err != nil {
		return nil, err
	}
	if err := waitActiveValidator(harness, seed, 60*time.Second); err != nil {
		return nil, err
	}
	target, err := nodeHeight(harness, seed)
	if err != nil {
		return nil, err
	}

	results := []bench.Result{}
	logf("starting the joining node and timing the sync of %d blocks", target)
	syncStart := time.Now()
	if err := harness.StartJoiner(joiner); err != nil {
		return nil, err
	}
	if err := harness.WaitHealthy(joiner, 90*time.Second); err != nil {
		return nil, err
	}
	if err := devnet.WaitUntil(120*time.Second, "joiner catch-up", func() bool {
		height, err := nodeHeight(harness, joiner)
		return err == nil && height >= target
	}); err != nil {
		return nil, err
	}
	syncSeconds := time.Since(syncStart).Seconds()
	if syncSeconds <= 0 {
		syncSeconds = 0.001
	}
	results = append(results, bench.Result{
		Name:    "network.sync_throughput",
		Unit:    "blocks/sec",
		Value:   float64(target) / syncSeconds,
		Seconds: syncSeconds,
		Extra:   map[string]any{"blocks": target, "restart_sync": false},
	})

	blockLatencies, err := measureBlockPropagation(harness, seed, joiner, 5)
	if err != nil {
		return nil, err
	}
	results = append(results, latencyResult("network.block_propagation", blockLatencies))

	txLatencies, err := measureTransactionLatency(harness, seed, joiner, 5)
	if err != nil {
		return nil, err
	}
	results = append(results, latencyResult("network.transaction_commit", txLatencies))
	return results, nil
}

// measureBlockPropagation polls both nodes in the same loop and records when
// each first observes the target height, so the reported latency is the
// observation gap (poll resolution 5ms) rather than the time after the block
// was already known. A block that is not produced within the window is
// retried instead of failing the whole lab (a busy host can momentarily stall
// production).
func measureBlockPropagation(harness *devnet.Harness, seed, joiner *devnet.NodeSpec, samples int) ([]float64, error) {
	latencies := []float64{}
	attempts := 0
	for len(latencies) < samples && attempts < samples*4 {
		attempts++
		startHeight, err := nodeHeight(harness, seed)
		if err != nil {
			return nil, err
		}
		target := startHeight + 1
		deadline := time.Now().Add(60 * time.Second)
		var seedSeen, joinerSeen time.Time
		for time.Now().Before(deadline) {
			if seedSeen.IsZero() {
				if height, err := nodeHeight(harness, seed); err == nil && height >= target {
					seedSeen = time.Now()
				}
			}
			if !seedSeen.IsZero() {
				if height, err := nodeHeight(harness, joiner); err == nil && height >= target {
					joinerSeen = time.Now()
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		if seedSeen.IsZero() || joinerSeen.IsZero() {
			continue
		}
		latencies = append(latencies, joinerSeen.Sub(seedSeen).Seconds())
	}
	if len(latencies) == 0 {
		return nil, fmt.Errorf("no block was observed by both nodes")
	}
	return latencies, nil
}

// measureTransactionLatency submits a transfer on the seed and times how long
// the joiner needs to expose the sender's next nonce (i.e. commit).
func measureTransactionLatency(harness *devnet.Harness, seed, joiner *devnet.NodeSpec, samples int) ([]float64, error) {
	latencies := []float64{}
	client := harness.Client(seed, true)
	joinerClient := harness.Client(joiner, false)
	nonce, err := client.Nonce(seed.Address)
	if err != nil {
		return nil, err
	}
	for index := 0; index < samples; index++ {
		start := time.Now()
		if _, err := client.Send(&nonce, map[string]any{
			"receiver": seed.Address,
			"value":    "1",
		}); err != nil {
			return nil, fmt.Errorf("submit: %w", err)
		}
		nonce++
		target := nonce
		if err := devnet.WaitUntil(60*time.Second, "joiner nonce", func() bool {
			seen, err := joinerClient.Nonce(seed.Address)
			return err == nil && seen >= target
		}); err != nil {
			return nil, err
		}
		latencies = append(latencies, time.Since(start).Seconds())
	}
	return latencies, nil
}

func nodeHeight(harness *devnet.Harness, spec *devnet.NodeSpec) (int64, error) {
	health, err := harness.Client(spec, false).Health()
	if err != nil {
		return 0, err
	}
	return devnet.ToInt64(health["height"]), nil
}

// waitActiveValidator polls /validators until the node's wallet is active.
func waitActiveValidator(harness *devnet.Harness, spec *devnet.NodeSpec, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		validators, err := harness.Client(spec, false).Validators()
		if err == nil {
			records, _ := validators["validators"].([]any)
			for _, raw := range records {
				if record, ok := raw.(map[string]any); ok {
					if address, _ := record["address"].(string); address == spec.Address {
						return nil
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s never became an active validator", spec.Name)
}

// latencyResult reports the median of the samples plus p90 and the raw list.
func latencyResult(name string, samples []float64) bench.Result {
	values := append([]float64{}, samples...)
	sort.Float64s(values)
	median := 0.0
	p90 := 0.0
	if len(values) > 0 {
		median = values[len(values)/2]
		index := int(float64(len(values)-1) * 0.9)
		p90 = values[index]
	}
	return bench.Result{
		Name:    name,
		Unit:    "seconds (median)",
		Value:   median,
		Ops:     int64(len(values)),
		Seconds: median,
		Extra: map[string]any{
			"p90_seconds": p90,
			"samples":     values,
		},
	}
}
