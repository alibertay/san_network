package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedAddressMapping(t *testing.T) {
	rest := seedRESTAddresses("seed.example.com,10.0.0.5:9000", 8000)
	if len(rest) != 2 || rest[0] != "seed.example.com:8000" || rest[1] != "10.0.0.5:9000" {
		t.Fatalf("seedRESTAddresses: %v", rest)
	}
	peers := seedPeerAddresses("seed.example.com,10.0.0.5:9001", 8770)
	if len(peers) != 2 || peers[0] != "seed.example.com:8770" || peers[1] != "10.0.0.5:9001" {
		t.Fatalf("seedPeerAddresses: %v", peers)
	}
}

func TestBuildChildEnvWideAreaFlags(t *testing.T) {
	opts := options{
		dataDir:       t.TempDir(),
		host:          "0.0.0.0",
		apiHost:       "0.0.0.0",
		seeds:         "seed.example.com",
		advertiseHost: "node1.example.com",
		peerCache:     "cache/peers.json",
		tlsCert:       "certs/node.crt",
		tlsKey:        "certs/node.key",
		tlsCA:         "certs/ca.crt",
		apiToken:      "tok",
		chainID:       "san-devnet-1",
		noRegistry:    true,
		genesisAmount: "10000",
	}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, true, "", nil))

	expectations := map[string]string{
		"SAN_DNS_SEEDS":          "seed.example.com",
		"SAN_BOOTSTRAP":          "seed.example.com:8770",
		"SAN_ADVERTISE_HOST":     "node1.example.com",
		"SAN_PEERS_CACHE":        "cache/peers.json",
		"SAN_TLS_CERT":           "certs/node.crt",
		"SAN_TLS_KEY":            "certs/node.key",
		"SAN_TLS_CA":             "certs/ca.crt",
		"SAN_API_TOKEN":          "tok",
		"SAN_API_HOST":           "0.0.0.0",
		"SAN_DISCOVERY":          "0",
		"SAN_PEER_REGISTRY":      "",
		"SANUP_CHILD":            "1",
		"SAN_GENESIS_ALLOCATION": "ab:10000",
	}
	for name, expected := range expectations {
		if got := env[name]; got != expected {
			t.Errorf("%s: got %q, want %q", name, got, expected)
		}
	}
}

func TestBuildChildEnvJoinIncludesBootstrap(t *testing.T) {
	opts := options{
		dataDir: t.TempDir(),
		host:    "127.0.0.1",
		apiHost: "0.0.0.0",
		seeds:   "seed.example.com",
		chainID: "san-devnet-1",
	}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports,
		false, "seed.example.com:8000", map[string]string{"SAN_CHAIN_ID": "san-devnet-1"}))
	if got := env["SAN_BOOTSTRAP"]; got != "seed.example.com:8000,seed.example.com:8770" {
		t.Fatalf("SAN_BOOTSTRAP: got %q", got)
	}
	if got := env["SAN_ADVERTISE_HOST"]; got != "127.0.0.1" {
		t.Fatalf("SAN_ADVERTISE_HOST: got %q", got)
	}
}

func TestBuildChildEnvPublicDevnetKeepsControllerCount(t *testing.T) {
	t.Setenv("SAN_PUBLIC_DEVNET", "1")
	t.Setenv("SAN_CONTROLLER_COUNT", "7")
	opts := options{dataDir: t.TempDir(), host: "0.0.0.0", chainID: "san-devnet-1"}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, false, "", nil))
	if got := env["SAN_CONTROLLER_COUNT"]; got != "7" {
		t.Fatalf("public-devnet SAN_CONTROLLER_COUNT: got %q, want the operator value 7", got)
	}
}

func TestBuildChildEnvDevModeForcesNoControllers(t *testing.T) {
	t.Setenv("SAN_PUBLIC_DEVNET", "")
	t.Setenv("SAN_CONTROLLER_COUNT", "7")
	opts := options{dataDir: t.TempDir(), host: "0.0.0.0", chainID: "san-devnet-1"}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, false, "", nil))
	if got := env["SAN_CONTROLLER_COUNT"]; got != "0" {
		t.Fatalf("dev-mode SAN_CONTROLLER_COUNT: got %q, want 0", got)
	}
}

func TestCertSubcommandGeneratesUsablePair(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runCert([]string{"--dir", dir, "--advertise-host", "node1.example.com,203.0.113.5"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runCert code=%d stderr=%s", code, stderr.String())
	}
	for _, name := range []string{"ca.crt", "ca.key", "node.crt", "node.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if !strings.Contains(stdout.String(), "SAN_TLS_CA=") {
		t.Fatalf("cert instructions missing SAN_TLS_CA: %s", stdout.String())
	}

	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	dnsNames := strings.Join(leaf.DNSNames, ",")
	if !strings.Contains(dnsNames, "node1.example.com") || !strings.Contains(dnsNames, "localhost") {
		t.Fatalf("node certificate SANs: %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) == 0 {
		t.Fatalf("node certificate has no IP SANs")
	}

	// The CA file parses and the node certificate chains to it.
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("ReadFile(ca.crt): %v", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatalf("ca.crt is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate(ca): %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "node1.example.com"}); err != nil {
		t.Fatalf("node certificate does not verify against the CA: %v", err)
	}
}

func envMap(entries []string) map[string]string {
	values := map[string]string{}
	for _, entry := range entries {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	return values
}
