package main

import (
	"io"
	"net"
	"testing"
)

func TestParseOptionsSeedAndStake(t *testing.T) {
	opts, code := parseOptions([]string{"--wallet", "0xabc", "--stake", "100"}, io.Discard, io.Discard)
	if code != -1 {
		t.Fatalf("parseOptions returned code %d", code)
	}
	if !opts.stakeProvided || opts.stake != "100" || opts.wallet != "0xabc" {
		t.Fatalf("unexpected options: %+v", opts)
	}
	if opts.seed.value != "auto" || opts.genesisAmount != "10000" || opts.host != "127.0.0.1" {
		t.Fatalf("unexpected defaults: %+v", opts)
	}

	opts, code = parseOptions([]string{"--seed", "--stake", "0"}, io.Discard, io.Discard)
	if code != -1 || opts.seed.value != "true" || !opts.stakeProvided {
		t.Fatalf("bare --seed: code=%d options=%+v", code, opts)
	}

	opts, code = parseOptions([]string{"--seed=false"}, io.Discard, io.Discard)
	if code != -1 || opts.seed.value != "false" {
		t.Fatalf("--seed=false: code=%d seed=%q", code, opts.seed.value)
	}

	if _, code = parseOptions([]string{"--seed=maybe"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("invalid --seed must fail with code 2, got %d", code)
	}
	if _, code = parseOptions([]string{"--seed", "false"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("--seed false must be rejected (use --seed=false), got code %d", code)
	}
}

func TestGenesisEnvFromPayload(t *testing.T) {
	payload := map[string]any{
		"chain_id": "san-devnet-1",
		"parameters": map[string]any{
			"block_reward":          "200000000",
			"min_validator_stake":   "0",
			"unbonding_period":      "0",
			"slash_bps":             "5000",
			"block_gas_limit":       "30000000",
			"proposer_timeout_ms":   "6000",
			"min_block_interval_ms": "1000",
		},
		"genesis_allocation": map[string]any{
			"0xbb": "500000000",
			"0xaa": "1000000000",
		},
	}
	env := genesisEnvFromPayload(payload)
	expectations := map[string]string{
		"SAN_CHAIN_ID":              "san-devnet-1",
		"SAN_BLOCK_REWARD":          "2",
		"SAN_MIN_VALIDATOR_STAKE":   "0",
		"SAN_UNBONDING_PERIOD":      "0",
		"SAN_SLASH_BPS":             "5000",
		"SAN_BLOCK_GAS_LIMIT":       "30000000",
		"SAN_PROPOSER_TIMEOUT":      "6",
		"SAN_MIN_BLOCK_INTERVAL_MS": "1000",
		"SAN_GENESIS_ALLOCATION":    "0xaa:10,0xbb:5",
	}
	for name, expected := range expectations {
		if got := env[name]; got != expected {
			t.Errorf("%s: got %q, want %q", name, got, expected)
		}
	}
}

func TestNormalizeBootstrap(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8000/": "127.0.0.1:8000",
		"127.0.0.1:8001":         "127.0.0.1:8001",
		" https://host:9000":     "host:9000",
	}
	for input, expected := range cases {
		if got := normalizeBootstrap(input); got != expected {
			t.Errorf("normalizeBootstrap(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestPickPortSkipsBusyPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port: %v", err)
	}
	defer listener.Close()
	busy := listener.Addr().(*net.TCPAddr).Port

	picked, err := pickPort("127.0.0.1", busy, map[int]bool{})
	if err != nil {
		t.Fatalf("pickPort: %v", err)
	}
	if picked == busy {
		t.Fatalf("pickPort returned the busy port %d", busy)
	}
}
