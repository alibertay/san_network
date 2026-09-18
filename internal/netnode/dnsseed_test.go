package netnode

import (
	"context"
	"fmt"
	"testing"
)

func TestSplitSeedAddress(t *testing.T) {
	cases := []struct {
		raw      string
		port     int
		wantHost string
		wantPort int
		ok       bool
	}{
		{"seed.example.com", 8770, "seed.example.com", 8770, true},
		{"seed.example.com:9000", 8770, "seed.example.com", 9000, true},
		{"127.0.0.1", 8770, "127.0.0.1", 8770, true},
		{"127.0.0.1:8123", 8770, "127.0.0.1", 8123, true},
		{"[::1]:8770", 8770, "::1", 8770, true},
		{"::1", 8770, "::1", 8770, true},
		{"", 8770, "", 0, false},
		{"host:notaport", 8770, "", 0, false},
		{"host:0", 8770, "", 0, false},
		{"host:70000", 8770, "", 0, false},
	}
	for _, testCase := range cases {
		host, port, ok := splitSeedAddress(testCase.raw, testCase.port)
		if ok != testCase.ok || host != testCase.wantHost || (ok && port != testCase.wantPort) {
			t.Errorf("splitSeedAddress(%q) = (%q, %d, %v), want (%q, %d, %v)",
				testCase.raw, host, port, ok, testCase.wantHost, testCase.wantPort, testCase.ok)
		}
	}
}

func TestResolveSeedHostsUsesResolver(t *testing.T) {
	original := dnsLookup
	defer func() { dnsLookup = original }()
	queries := []string{}
	dnsLookup = func(ctx context.Context, host string) ([]string, error) {
		queries = append(queries, host)
		switch host {
		case "seed.example.com":
			return []string{"203.0.113.20", "203.0.113.10", "203.0.113.10"}, nil
		default:
			return nil, fmt.Errorf("no such host")
		}
	}

	addresses, err := resolveSeedHosts(context.Background(), "seed.example.com", 8770)
	if err != nil {
		t.Fatalf("resolveSeedHosts: %v", err)
	}
	want := []string{"203.0.113.10:8770", "203.0.113.20:8770"}
	if len(addresses) != len(want) || addresses[0] != want[0] || addresses[1] != want[1] {
		t.Fatalf("resolved %v, want %v", addresses, want)
	}
	if len(queries) != 1 || queries[0] != "seed.example.com" {
		t.Fatalf("unexpected resolver queries: %v", queries)
	}

	if _, err := resolveSeedHosts(context.Background(), "missing.example.com", 8770); err == nil {
		t.Fatalf("resolution failure must be reported")
	}
	if _, err := resolveSeedHosts(context.Background(), "seed.example.com:9000", 8770); err != nil {
		t.Fatalf("host:port seed: %v", err)
	}
}

func TestResolveDNSSeedsFeedsAddrman(t *testing.T) {
	original := dnsLookup
	defer func() { dnsLookup = original }()
	dnsLookup = func(ctx context.Context, host string) ([]string, error) {
		return []string{"203.0.113.7"}, nil
	}

	config := testConfig()
	config.DNSSeeds = []string{"seed.example.com", "198.51.100.9:9999"}
	config.PeerPort = 8770
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	node.addrman = NewAddrManager(config.MaxAddrEntries, "")

	added := node.resolveDNSSeeds(context.Background())
	if added != 2 {
		t.Fatalf("resolveDNSSeeds added %d addresses, want 2", added)
	}
	resolved := addrRecord("203.0.113.7", 8770)
	if !node.addrman.Has(resolved) {
		t.Fatalf("resolved address missing from the address manager: %v", node.addrman.Snapshot())
	}
	explicitPort := addrRecord("198.51.100.9", 9999)
	if !node.addrman.Has(explicitPort) {
		t.Fatalf("host:port seed missing from the address manager: %v", node.addrman.Snapshot())
	}
}
