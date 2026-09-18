package api

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/canonical"
)

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
