package netnode

import (
	"os"
	"path/filepath"
	"testing"
)

func addrRecord(host string, peerPort int64) map[string]any {
	return map[string]any{
		"host":      host,
		"peer_port": peerPort,
		"p2p_port":  int64(8765),
		"api_port":  int64(8000),
		"chain_id":  "san-devnet-1",
	}
}

func TestAddrManagerAddDedupeAndEvict(t *testing.T) {
	manager := NewAddrManager(3, "")
	manager.now = func() float64 { return 1000 }

	for i, host := range []string{"a.example", "b.example", "c.example", "d.example", "e.example"} {
		if !manager.Add(addrRecord(host, int64(8770+i)), AddrSourceSeed) {
			t.Fatalf("Add(%s) reported no new entry", host)
		}
	}
	if manager.Len() != 3 {
		t.Fatalf("MaxAddrEntries not enforced: got %d, want 3", manager.Len())
	}
	// The two oldest untried entries were evicted first.
	if manager.Has(addrRecord("a.example", 8770)) || manager.Has(addrRecord("b.example", 8771)) {
		t.Fatalf("oldest entries were not evicted: %v", manager.Snapshot())
	}
	if !manager.Has(addrRecord("e.example", 8774)) {
		t.Fatalf("newest entry missing after eviction")
	}

	before := manager.Len()
	if manager.Add(addrRecord("e.example", 8774), AddrSourceGossip) {
		t.Fatalf("duplicate address reported as new")
	}
	if manager.Len() != before {
		t.Fatalf("duplicate address grew the manager: %d -> %d", before, manager.Len())
	}
}

func TestAddrManagerValidatorAndSelfFilter(t *testing.T) {
	manager := NewAddrManager(8, "")
	manager.SetValidator(func(record map[string]any) bool { return record["chain_id"] == "san-devnet-1" })
	manager.SetSelfFilter(func(record map[string]any) bool { return record["host"] == "self.example" })

	invalid := addrRecord("gossip.example", 8770)
	invalid["chain_id"] = "other-chain"
	if manager.Add(invalid, AddrSourceGossip) {
		t.Fatalf("invalid gossip record was stored")
	}
	if !manager.Add(addrRecord("gossip.example", 8770), AddrSourceBootstrap) {
		t.Fatalf("bootstrap records must not go through the gossip validator")
	}
	if manager.Add(addrRecord("self.example", 8770), AddrSourceSeed) {
		t.Fatalf("self record was stored")
	}
	if manager.Len() != 1 {
		t.Fatalf("unexpected manager size: %d", manager.Len())
	}
}

func TestAddrManagerBackoffAfterFailure(t *testing.T) {
	manager := NewAddrManager(8, "")
	now := 1000.0
	manager.now = func() float64 { return now }
	record := addrRecord("flaky.example", 8770)
	manager.Add(record, AddrSourceGossip)

	if !manager.Has(record) {
		t.Fatalf("record missing")
	}
	if selected := manager.Select(4); len(selected) != 1 {
		t.Fatalf("fresh entry must be selectable: %v", selected)
	}
	manager.MarkTried(record)
	manager.MarkFailure(record)
	if failures := mustInfo(t, manager, record).Failures; failures != 1 {
		t.Fatalf("failures: got %d, want 1", failures)
	}
	if selected := manager.Select(4); len(selected) != 0 {
		t.Fatalf("entry in backoff must not be selectable: %v", selected)
	}
	now += backoffFor(1).Seconds() + 1
	if selected := manager.Select(4); len(selected) != 1 {
		t.Fatalf("entry must be selectable again after the backoff: %v", selected)
	}
	manager.MarkSuccess(record)
	if info := mustInfo(t, manager, record); info.Failures != 0 || !info.Tried {
		t.Fatalf("MarkSuccess did not reset the entry: %+v", info)
	}
}

func TestAddrManagerEvictionAfterMaxFailures(t *testing.T) {
	manager := NewAddrManager(8, "")
	record := addrRecord("gone.example", 8770)
	manager.Add(record, AddrSourceBootstrap)
	for i := 1; i <= maxAddrFailures; i++ {
		removed := manager.MarkFailure(record)
		if i < maxAddrFailures && removed {
			t.Fatalf("entry evicted early at failure %d", i)
		}
		if i == maxAddrFailures && !removed {
			t.Fatalf("entry was not evicted at failure %d", i)
		}
	}
	if manager.Has(record) {
		t.Fatalf("entry still present after %d failures", maxAddrFailures)
	}
}

func TestAddrManagerPersistAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers-cache.json")
	manager := NewAddrManager(8, path)
	manager.now = func() float64 { return 5000 }
	first := addrRecord("alpha.example", 8771)
	second := addrRecord("beta.example", 8772)
	manager.Add(first, AddrSourceSeed)
	manager.Add(second, AddrSourceGossip)
	manager.MarkTried(second)
	manager.MarkFailure(second)
	if !manager.Dirty() {
		t.Fatalf("manager should be dirty before Save")
	}
	if err := manager.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if manager.Dirty() {
		t.Fatalf("Save did not clear the dirty flag")
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("cache file missing or empty: %v", err)
	}

	loaded := NewAddrManager(8, path)
	loaded.now = func() float64 { return 5000 }
	count, err := loaded.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if count != 2 || loaded.Len() != 2 {
		t.Fatalf("Load: count=%d len=%d, want 2", count, loaded.Len())
	}
	info, ok := loaded.info(first)
	if !ok || info.Source != AddrSourceSeed {
		t.Fatalf("persisted source not restored: %+v", info)
	}
	info, ok = loaded.info(second)
	if !ok || !info.Tried || info.Failures != 1 || info.Source != AddrSourceGossip {
		t.Fatalf("persisted bookkeeping not restored: %+v", info)
	}
}

func TestAddrManagerLoadCorruptCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers-cache.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	manager := NewAddrManager(8, path)
	if _, err := manager.Load(); err == nil {
		t.Fatalf("corrupt cache must return an error")
	}
}

func TestAddrManagerSelectPrefersTriedAndFreshest(t *testing.T) {
	manager := NewAddrManager(8, "")
	now := 1000.0
	manager.now = func() float64 { return now }
	fresh := addrRecord("fresh.example", 8770)
	tried := addrRecord("tried.example", 8771)
	manager.Add(fresh, AddrSourceBootstrap)
	manager.Add(tried, AddrSourceBootstrap)
	manager.MarkTried(tried)
	selected := manager.Select(1)
	if len(selected) != 1 || stringValue(selected[0]["host"]) != "tried.example" {
		t.Fatalf("tried entries must be selected first: %v", selected)
	}
}

func mustInfo(t *testing.T, manager *AddrManager, record map[string]any) addrEntry {
	t.Helper()
	info, ok := manager.info(record)
	if !ok {
		t.Fatalf("entry %v not found", record)
	}
	return info
}
