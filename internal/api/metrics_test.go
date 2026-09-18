package api_test

import (
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

// expectedZeroMetrics is the exact app/metrics.py output for an empty
// snapshot (counter names/order and gauge names/order from app/metrics.py).
const expectedZeroMetrics = `# TYPE san_blocks_committed counter
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
# TYPE san_peer_penalties counter
san_peer_penalties 0
# TYPE san_peers_banned counter
san_peers_banned 0
# TYPE san_peers_rejected_inbound counter
san_peers_rejected_inbound 0
# TYPE san_peers_rejected_subnet counter
san_peers_rejected_subnet 0
# TYPE san_peers_rejected_banned counter
san_peers_rejected_banned 0
# TYPE san_peer_malformed_messages counter
san_peer_malformed_messages 0
# TYPE san_peer_rate_limit_hits counter
san_peer_rate_limit_hits 0
# TYPE san_peer_invalid_records counter
san_peer_invalid_records 0
# TYPE san_mempool_rejected counter
san_mempool_rejected 0
# TYPE san_orphans_evicted counter
san_orphans_evicted 0
# TYPE san_block_requests_rejected counter
san_block_requests_rejected 0
# TYPE san_staged_votes_rejected counter
san_staged_votes_rejected 0
# TYPE san_peers_rejected_table counter
san_peers_rejected_table 0
# TYPE san_vote_seen_cache_resets counter
san_vote_seen_cache_resets 0
# TYPE san_http_body_rejected counter
san_http_body_rejected 0
# TYPE san_height gauge
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
# TYPE san_controllers_target gauge
san_controllers_target 0
# TYPE san_peer_bans_active gauge
san_peer_bans_active 0
`

func TestRenderMetricsZeroSnapshot(t *testing.T) {
	if got := api.RenderMetrics(map[string]any{}); got != expectedZeroMetrics {
		t.Errorf("zero snapshot mismatch:\n got:\n%s\nwant:\n%s", got, expectedZeroMetrics)
	}
	if !strings.HasSuffix(expectedZeroMetrics, "\n") {
		t.Errorf("exposition must end with a newline")
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
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %q in:\n%s", expected, text)
		}
	}
}
