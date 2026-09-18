package netnode

import (
	"context"
	"testing"
)

func TestMaintainOutboundUsesStubDialerAndBackoff(t *testing.T) {
	config := testConfig()
	config.OutboundPeers = 2
	config.RequireBlockSig = false
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	now := 1000.0
	node.addrman = NewAddrManager(8, "")
	node.addrman.now = func() float64 { return now }

	records := []map[string]any{
		addrRecord("one.example", 8770),
		addrRecord("two.example", 8771),
		addrRecord("three.example", 8772),
	}
	for _, record := range records {
		node.addrman.Add(record, AddrSourceBootstrap)
	}

	attempts := map[string]int{}
	node.outboundDial = func(ctx context.Context, record map[string]any) bool {
		host := stringValue(record["host"])
		attempts[host]++
		if host == "one.example" {
			node.AddPeer(record)
			return true
		}
		return false
	}

	node.maintainOutbound(context.Background())
	total := 0
	for _, count := range attempts {
		total += count
	}
	if total != 2 {
		t.Fatalf("first pass made %d attempt(s), want 2 (OutboundPeers): %v", total, attempts)
	}
	if attempts["one.example"] != 1 || len(node.Peers()) != 1 {
		t.Fatalf("successful dial must add the peer and stop: attempts=%v peers=%d", attempts, len(node.Peers()))
	}
	if info := mustInfo(t, node.addrman, records[0]); !info.Tried || info.Failures != 0 {
		t.Fatalf("successful entry bookkeeping wrong: %+v", info)
	}

	attempts = map[string]int{}
	node.maintainOutbound(context.Background())
	if attempts["three.example"] != 0 {
		t.Fatalf("failed entry must not be retried while in backoff: %v", attempts)
	}
	total = 0
	for _, count := range attempts {
		total += count
	}
	if total != 1 {
		t.Fatalf("second pass should fill the remaining outbound slot: %v", attempts)
	}

	now += backoffFor(1).Seconds() + 1
	attempts = map[string]int{}
	node.maintainOutbound(context.Background())
	if attempts["three.example"] != 1 {
		t.Fatalf("entry must be selectable again after backoff: %v", attempts)
	}
}

func TestOutboundEvictsAfterRepeatedFailures(t *testing.T) {
	config := testConfig()
	config.OutboundPeers = 1
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	now := 2000.0
	node.addrman = NewAddrManager(8, "")
	node.addrman.now = func() float64 { return now }
	record := addrRecord("dead.example", 8770)
	node.addrman.Add(record, AddrSourceBootstrap)
	node.outboundDial = func(context.Context, map[string]any) bool { return false }

	for i := 1; i <= maxAddrFailures; i++ {
		node.maintainOutbound(context.Background())
		now += backoffFor(i).Seconds() + 1
	}
	if node.addrman.Has(record) {
		t.Fatalf("address not evicted after %d failures", maxAddrFailures)
	}
}

func TestBackoffForGrowsAndCaps(t *testing.T) {
	if backoffFor(0) != 0 {
		t.Fatalf("zero failures must mean no backoff")
	}
	if backoffFor(1) != addrBackoffBase {
		t.Fatalf("first failure backoff: got %v", backoffFor(1))
	}
	if backoffFor(2) <= backoffFor(1) {
		t.Fatalf("backoff must grow: %v then %v", backoffFor(1), backoffFor(2))
	}
	if backoffFor(30) != addrBackoffMax {
		t.Fatalf("backoff must cap at %v, got %v", addrBackoffMax, backoffFor(30))
	}
}
