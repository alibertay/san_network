package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/genesis"
)

func loadDeployGenesis(t *testing.T) *genesis.Spec {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "genesis.json")
	spec, err := genesis.Load(path)
	if err != nil {
		t.Fatalf("genesis.Load(%s): %v", path, err)
	}
	return spec
}

func TestParseOptionsGenesisAndPublicFlags(t *testing.T) {
	opts, code := parseOptions([]string{"--genesis-file", "deploy/genesis.json", "--public-devnet", "--allow-insecure-public"}, discard(), discard())
	if code != -1 {
		t.Fatalf("parseOptions returned code %d", code)
	}
	if opts.genesisFile != "deploy/genesis.json" || !opts.publicDevnet || !opts.allowInsecure {
		t.Fatalf("genesis/public options not parsed: %+v", opts)
	}
}

// discard is a tiny io.Writer alias so the table stays readable.
func discard() *strings.Builder { return &strings.Builder{} }

// unsetEnv removes a variable for the duration of the test (t.Setenv cannot
// express "unset", and buildChildEnv branches on LookupEnv).
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	previous, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, previous)
		}
	})
}

func TestBuildChildEnvUsesGenesisFile(t *testing.T) {
	spec := loadDeployGenesis(t)
	genesisEnv, err := spec.Environment()
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	t.Setenv("SAN_DB_BACKEND", "lmdb")
	t.Setenv("SAN_CONTROLLER_COUNT", "5")
	opts := options{
		dataDir:      t.TempDir(),
		host:         "0.0.0.0",
		chainID:      spec.ChainID,
		genesisFile:  "deploy/genesis.json",
		publicDevnet: true,
	}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, true, "", genesisEnv))

	expectations := map[string]string{
		"SAN_GENESIS_FILE":        "deploy/genesis.json",
		"SAN_CHAIN_ID":            spec.ChainID,
		"SAN_GENESIS_FINGERPRINT": spec.Fingerprint,
		"SAN_PUBLIC_DEVNET":       "1",
		"SAN_DB_BACKEND":          "lmdb",
		"SAN_CONTROLLER_COUNT":    "5",
	}
	for name, expected := range expectations {
		if got := env[name]; got != expected {
			t.Errorf("%s: got %q, want %q", name, got, expected)
		}
	}
	// The canonical file replaces the dynamic founder premine.
	if got := env["SAN_GENESIS_ALLOCATION"]; got != genesisEnv["SAN_GENESIS_ALLOCATION"] {
		t.Errorf("SAN_GENESIS_ALLOCATION: got %q, want the file allocation %q", got, genesisEnv["SAN_GENESIS_ALLOCATION"])
	}
	if env["SAN_BLOCK_REWARD"] != "2" || env["SAN_MIN_BLOCK_INTERVAL_MS"] != "1000" {
		t.Errorf("genesis parameters were not passed through: %v", env)
	}
}

func TestBuildChildEnvDevModeStillForcesMemoryAndNoControllers(t *testing.T) {
	unsetEnv(t, "SAN_DB_BACKEND")
	t.Setenv("SAN_CONTROLLER_COUNT", "5")
	opts := options{dataDir: t.TempDir(), host: "127.0.0.1", chainID: "san-devnet-1", genesisAmount: "10000"}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, true, "", nil))
	if env["SAN_DB_BACKEND"] != "memory" {
		t.Fatalf("dev-mode SAN_DB_BACKEND: %q, want memory", env["SAN_DB_BACKEND"])
	}
	if env["SAN_CONTROLLER_COUNT"] != "0" {
		t.Fatalf("dev-mode SAN_CONTROLLER_COUNT: %q, want 0", env["SAN_CONTROLLER_COUNT"])
	}
	if env["SAN_GENESIS_ALLOCATION"] != "ab:10000" {
		t.Fatalf("dev-mode founder premine: %q", env["SAN_GENESIS_ALLOCATION"])
	}
}

func TestValidatePublicProfileMatrix(t *testing.T) {
	valid := options{
		publicDevnet: true,
		genesisFile:  "deploy/genesis.json",
		apiHost:      "0.0.0.0",
		apiToken:     "token",
	}
	t.Setenv("SAN_DB_BACKEND", "lmdb")
	t.Setenv("SAN_GENESIS_FINGERPRINT", "")
	t.Setenv("SAN_FAUCET", "")
	if err := validatePublicProfile(valid); err != nil {
		t.Fatalf("valid public profile rejected: %v", err)
	}

	cases := []struct {
		name string
		opts options
		env  map[string]string
	}{
		{"missing genesis file", options{publicDevnet: true, apiHost: "127.0.0.1"}, nil},
		{"memory backend", valid, map[string]string{"SAN_DB_BACKEND": "memory"}},
		{"unauthenticated public API", options{publicDevnet: true, genesisFile: "deploy/genesis.json", apiHost: "0.0.0.0"}, nil},
		{"unauthenticated faucet", func() options {
			value := valid
			value.faucet = true
			value.apiHost = "127.0.0.1"
			value.apiToken = ""
			return value
		}(), nil},
	}
	for _, testCase := range cases {
		for name, value := range testCase.env {
			t.Setenv(name, value)
		}
		if err := validatePublicProfile(testCase.opts); err == nil {
			t.Errorf("%s: insecure public profile was accepted", testCase.name)
		}
		overridden := testCase.opts
		overridden.allowInsecure = true
		if err := validatePublicProfile(overridden); err != nil {
			t.Errorf("%s: --allow-insecure-public did not downgrade: %v", testCase.name, err)
		}
		// Reset the environment overrides for the next case.
		t.Setenv("SAN_DB_BACKEND", "lmdb")
		t.Setenv("SAN_GENESIS_FINGERPRINT", "")
	}

	loopback := valid
	loopback.apiHost = "127.0.0.1"
	loopback.apiToken = ""
	if err := validatePublicProfile(loopback); err != nil {
		t.Errorf("loopback API without a token must be accepted: %v", err)
	}
	t.Setenv("SAN_FAUCET", "1")
	withToken := valid
	withToken.faucet = true
	if err := validatePublicProfile(withToken); err != nil {
		t.Errorf("authenticated faucet must be accepted: %v", err)
	}
}

func TestGenesisEnvFromPayloadCarriesFingerprint(t *testing.T) {
	payload := map[string]any{
		"chain_id":            "san-devnet-1",
		"genesis_fingerprint": "abc123",
		"parameters": map[string]any{
			"block_reward":        "200000000",
			"min_validator_stake": "0",
		},
		"genesis_allocation": map[string]any{"0xaa": "1000000000"},
	}
	env := genesisEnvFromPayload(payload)
	if env["SAN_GENESIS_FINGERPRINT"] != "abc123" {
		t.Fatalf("SAN_GENESIS_FINGERPRINT: %q", env["SAN_GENESIS_FINGERPRINT"])
	}
	if env["SAN_CHAIN_ID"] != "san-devnet-1" {
		t.Fatalf("SAN_CHAIN_ID: %q", env["SAN_CHAIN_ID"])
	}
}

func TestFetchGenesisEnvPinsFingerprint(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	payload := map[string]any{
		"chain_id":            "san-devnet-1",
		"genesis_fingerprint": fingerprint,
		"parameters": map[string]any{
			"block_reward": "200000000",
		},
		"genesis_allocation": map[string]any{"0xaa": "1000000000"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/genesis" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	env, err := fetchGenesisEnv(server.URL, "", fingerprint)
	if err != nil {
		t.Fatalf("fetchGenesisEnv: %v", err)
	}
	if env["SAN_GENESIS_FINGERPRINT"] != fingerprint {
		t.Fatalf("SAN_GENESIS_FINGERPRINT: %q", env["SAN_GENESIS_FINGERPRINT"])
	}

	if _, err := fetchGenesisEnv(server.URL, "", strings.Repeat("cd", 32)); err == nil {
		t.Fatalf("mismatched seed fingerprint was accepted")
	} else if !strings.Contains(err.Error(), "genesis mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}

	delete(payload, "genesis_fingerprint")
	if _, err := fetchGenesisEnv(server.URL, "", fingerprint); err == nil {
		t.Fatalf("seed without a fingerprint was accepted for a pinned join")
	} else if !strings.Contains(err.Error(), "does not report") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trust-on-first-use (no pinned fingerprint) still works when the seed
	// reports an allocation.
	if _, err := fetchGenesisEnv(server.URL, "", ""); err != nil {
		t.Fatalf("TOFU fetch failed: %v", err)
	}
}
