package api_test

import (
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

// expectedPythonParityCounters is the exact counter section prefix of
// app/metrics.py for an empty snapshot. Go appends its local-only counters
// after it, then the gauge section.
const expectedPythonParityCounters = `# TYPE san_blocks_committed counter
san_blocks_committed 0
# TYPE san_transactions_committed counter
san_transactions_committed 0
# TYPE san_votes_received counter
san_votes_received 0
# TYPE san_votes_seen counter
san_votes_seen 0
# TYPE san_vote_messages_received counter
san_vote_messages_received 0
# TYPE san_votes_dropped_invalid counter
san_votes_dropped_invalid 0
# TYPE san_votes_dropped_height counter
san_votes_dropped_height 0
# TYPE san_votes_dropped_voter counter
san_votes_dropped_voter 0
# TYPE san_votes_dropped_duplicate counter
san_votes_dropped_duplicate 0
# TYPE san_votes_dropped_equivocation counter
san_votes_dropped_equivocation 0
# TYPE san_votes_broadcast counter
san_votes_broadcast 0
# TYPE san_votes_unbroadcast counter
san_votes_unbroadcast 0
# TYPE san_reorgs counter
san_reorgs 0
# TYPE san_slashing_events counter
san_slashing_events 0
# TYPE san_governance_changes counter
san_governance_changes 0
`

// expectedPythonParityGauges is the exact gauge section of app/metrics.py; the
// Go-only gauges are appended after it.
const expectedPythonParityGauges = `# TYPE san_height gauge
san_height 0
# TYPE san_finalized_height gauge
san_finalized_height 0
# TYPE san_peers gauge
san_peers 0
# TYPE san_controllers gauge
san_controllers 0
# TYPE san_mempool gauge
san_mempool 0
# TYPE san_validators gauge
san_validators 0
# TYPE san_total_stake_units gauge
san_total_stake_units 0
# TYPE san_total_slashed gauge
san_total_slashed 0
# TYPE san_total_burned gauge
san_total_burned 0
# TYPE san_base_fee gauge
san_base_fee 0
# TYPE san_contracts gauge
san_contracts 0
# TYPE san_orphans gauge
san_orphans 0
`

// expectedGoOnlyCounters must be present after the Python prefix.
var expectedGoOnlyCounters = []string{
	"san_peer_penalties", "san_peers_banned", "san_peers_rejected_inbound",
	"san_peers_rejected_subnet", "san_peers_rejected_banned",
	"san_peer_malformed_messages", "san_peer_rate_limit_hits",
	"san_peer_invalid_records", "san_mempool_rejected", "san_orphans_evicted",
	"san_block_requests_rejected", "san_staged_votes_rejected",
	"san_peers_rejected_table", "san_vote_seen_cache_resets",
	"san_http_body_rejected", "san_transactions_accepted",
	"san_transactions_rejected", "san_transactions_duplicated",
	"san_execution_failures", "san_gas_used", "san_validation_failures",
	"san_peers_reconnects", "san_handshakes_failed", "san_peer_invalid_messages",
	"san_sync_attempts", "san_sync_failures", "san_bytes_sent",
	"san_bytes_received", "san_controller_approvals",
	"san_controller_failures", "san_api_rate_limited",
}

// expectedGoOnlyGauges must be present after the Python prefix.
var expectedGoOnlyGauges = []string{
	"san_controllers_target", "san_peer_bans_active", "san_peers_inbound",
	"san_peers_outbound", "san_reorg_depth", "san_proposer_round",
	"san_mempool_bytes", "san_pending_finality", "san_go_goroutines",
	"san_go_memory_alloc_bytes", "san_go_memory_sys_bytes", "san_go_gc_cycles",
	"san_uptime_seconds",
}

func TestRenderMetricsZeroSnapshotKeepsPythonParityPrefix(t *testing.T) {
	if !strings.HasPrefix(expectedPythonParityCounters, "# TYPE san_blocks_committed counter") {
		t.Fatal("parity fixture must start with the first Python counter")
	}
	got := api.RenderMetrics(map[string]any{})
	if !strings.HasPrefix(got, expectedPythonParityCounters) {
		t.Errorf("Python parity counter prefix changed:\n got:\n%s", firstLines(got, 32))
	}
	if !strings.Contains(got, expectedPythonParityGauges+"# TYPE san_controllers_target gauge") {
		t.Errorf("Python parity gauge block changed:\n got:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("exposition must end with a newline")
	}
}

func TestRenderMetricsExposesGoOnlyMetrics(t *testing.T) {
	text := api.RenderMetrics(map[string]any{})
	for _, name := range append(append([]string{}, expectedGoOnlyCounters...), expectedGoOnlyGauges...) {
		if !strings.Contains(text, "# TYPE "+name+" ") {
			t.Errorf("missing metric type declaration for %s", name)
		}
	}
}

func TestRenderMetricsValues(t *testing.T) {
	snapshot := map[string]any{
		"blocks_committed":       int64(7),
		"transactions_committed": int64(3),
		"votes_received":         int64(0),
		"height":                 int64(12),
		"finalized_height":       int64(10),
		"peers":                  int64(2),
		"controllers":            int64(1),
		"mempool":                int64(4),
		"validators":             int64(1),
		"total_stake_units":      int64(100_000_000_000),
		"total_slashed":          int64(5),
		"total_burned":           int64(6),
		"base_fee":               int64(2),
		"contracts":              int64(1),
		"orphans":                int64(0),
		"transactions_accepted":  int64(9),
		"gas_used":               int64(1234),
		"go_goroutines":          int64(42),
		"uptime_seconds":         12.5,
	}
	text := api.RenderMetrics(snapshot)
	for _, expected := range []string{
		"san_blocks_committed 7",
		"san_transactions_committed 3",
		"san_votes_received 0",
		"# TYPE san_height gauge",
		"san_height 12",
		"san_finalized_height 10",
		"san_total_stake_units 100000000000",
		"san_contracts 1",
		"san_transactions_accepted 9",
		"san_gas_used 1234",
		"san_go_goroutines 42",
		"san_uptime_seconds 12.5",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in:\n%s", expected, text)
		}
	}
}

func firstLines(text string, count int) string {
	lines := strings.SplitN(text, "\n", count+1)
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
