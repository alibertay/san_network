package api

import (
	"encoding/json"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
)

// processStarted is the wall-clock start used for the uptime gauge.
var processStarted = time.Now()

// RuntimeSnapshot renders the standard-library runtime metrics as gauges.
// It uses runtime.ReadMemStats directly (no external dependency) so the
// scraped text stays Prometheus friendly.
func RuntimeSnapshot() map[string]any {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return map[string]any{
		"go_goroutines":         int64(runtime.NumGoroutine()),
		"go_memory_alloc_bytes": int64(memory.Alloc),
		"go_memory_sys_bytes":   int64(memory.Sys),
		"go_gc_cycles":          int64(memory.NumGC),
		"uptime_seconds":        time.Since(processStarted).Seconds(),
	}
}

// CounterNames and GaugeNames mirror app/metrics.py order exactly for the
// shared metrics; Go-only local peer-management metrics are appended after
// them so Python parity for the common prefix is preserved.
var counterNames = []string{
	"blocks_committed",
	"transactions_committed",
	"votes_received",
	"votes_seen",
	"vote_messages_received",
	"votes_dropped_invalid",
	"votes_dropped_height",
	"votes_dropped_voter",
	"votes_dropped_duplicate",
	"votes_dropped_equivocation",
	"votes_broadcast",
	"votes_unbroadcast",
	"reorgs",
	"slashing_events",
	"governance_changes",
	"peer_penalties",
	"peers_banned",
	"peers_rejected_inbound",
	"peers_rejected_subnet",
	"peers_rejected_banned",
	"peer_malformed_messages",
	"peer_rate_limit_hits",
	"peer_invalid_records",
	// Batch C resource-limit rejections (Go-only, appended so the Python
	// parity prefix above is unchanged).
	"mempool_rejected",
	"orphans_evicted",
	"block_requests_rejected",
	"staged_votes_rejected",
	"peers_rejected_table",
	"vote_seen_cache_resets",
	"http_body_rejected",
	// Batch D observability: transaction, chain, network and consensus
	// counters (Go-only, appended after the Batch C block).
	"transactions_accepted",
	"transactions_rejected",
	"transactions_duplicated",
	"execution_failures",
	"gas_used",
	"validation_failures",
	"peers_reconnects",
	"handshakes_failed",
	"peer_invalid_messages",
	"sync_attempts",
	"sync_failures",
	"bytes_sent",
	"bytes_received",
	"controller_approvals",
	"controller_failures",
	"api_rate_limited",
}

var gaugeNames = []string{
	"height",
	"finalized_height",
	"peers",
	"controllers",
	"mempool",
	"validators",
	"total_stake_units",
	"total_slashed",
	"total_burned",
	"base_fee",
	"contracts",
	"orphans",
	"controllers_target",
	"peer_bans_active",
	// Batch D observability gauges.
	"peers_inbound",
	"peers_outbound",
	"reorg_depth",
	"proposer_round",
	"mempool_bytes",
	"pending_finality",
	// Runtime gauges from the standard library runtime/metrics surface.
	"go_goroutines",
	"go_memory_alloc_bytes",
	"go_memory_sys_bytes",
	"go_gc_cycles",
	"uptime_seconds",
}

// RenderMetrics reproduces app.metrics.render_metrics line for line.
func RenderMetrics(snapshot map[string]any) string {
	lines := make([]string, 0, (len(counterNames)+len(gaugeNames))*2)

	for _, name := range counterNames {
		metric := "san_" + name
		lines = append(lines, "# TYPE "+metric+" counter")
		lines = append(lines, metric+" "+formatCounter(snapshot[name]))
	}

	for _, name := range gaugeNames {
		metric := "san_" + name
		lines = append(lines, "# TYPE "+metric+" gauge")
		lines = append(lines, metric+" "+formatGauge(snapshot[name]))
	}

	return strings.Join(lines, "\n") + "\n"
}

// formatCounter is Python's int(snapshot.get(name, 0) or 0).
func formatCounter(value any) string {
	return strconv.FormatInt(metricInt(value), 10)
}

// formatGauge is Python's str(snapshot.get(name, 0)): missing values print 0.
func formatGauge(value any) string {
	switch typed := value.(type) {
	case nil:
		return "0"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return typed
	case float32:
		return metricFloat(float64(typed))
	case float64:
		return metricFloat(typed)
	case json.Number:
		return typed.String()
	case *big.Int:
		if typed == nil {
			return "0"
		}
		return typed.String()
	default:
		return strconv.FormatInt(metricInt(value), 10)
	}
}

func metricFloat(value float64) string {
	text, err := canonical.MarshalString(value)
	if err != nil {
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
	return text
}

func metricInt(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case bool:
		if typed {
			return 1
		}
		return 0
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			asFloat, floatErr := typed.Float64()
			if floatErr != nil {
				return 0
			}
			return int64(asFloat)
		}
		return parsed
	case *big.Int:
		if typed == nil {
			return 0
		}
		if !typed.IsInt64() {
			return 0
		}
		return typed.Int64()
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}
