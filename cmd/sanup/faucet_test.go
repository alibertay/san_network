package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertSubcommandCAOnlyAndSharedCA(t *testing.T) {
	caDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runCert([]string{"--ca-only", "--dir", caDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("runCert --ca-only code=%d stderr=%s", code, stderr.String())
	}
	for _, name := range []string{"ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(caDir, name)); err != nil {
			t.Fatalf("missing CA file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(caDir, "node.crt")); err == nil {
		t.Fatalf("--ca-only must not write a node certificate")
	}
	caBefore, err := os.ReadFile(filepath.Join(caDir, "ca.crt"))
	if err != nil {
		t.Fatalf("ReadFile(ca.crt): %v", err)
	}
	if !strings.Contains(stdout.String(), "NEVER leaves the CA machine") {
		t.Fatalf("CA instructions missing the distribution warning: %s", stdout.String())
	}

	nodeDir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	if code := runCert([]string{"--ca-dir", caDir, "--dir", nodeDir, "--advertise-host", "node1.example.com"}, &stdout, &stderr); code != 0 {
		t.Fatalf("runCert --ca-dir code=%d stderr=%s", code, stderr.String())
	}
	for _, name := range []string{"node.crt", "node.key"} {
		if _, err := os.Stat(filepath.Join(nodeDir, name)); err != nil {
			t.Fatalf("missing node file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(nodeDir, "ca.key")); err == nil {
		t.Fatalf("--ca-dir must not copy the CA key into the node directory")
	}
	caAfter, err := os.ReadFile(filepath.Join(caDir, "ca.crt"))
	if err != nil {
		t.Fatalf("ReadFile(ca.crt) after signing: %v", err)
	}
	if !bytes.Equal(caBefore, caAfter) {
		t.Fatalf("ca.crt changed while signing node certificates")
	}
}

func TestCertReusesExistingCA(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runCert([]string{"--dir", dir, "--advertise-host", "node1.example.com"}, &stdout, &stderr); code != 0 {
		t.Fatalf("first runCert code=%d stderr=%s", code, stderr.String())
	}
	caBefore, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	nodeBefore, _ := os.ReadFile(filepath.Join(dir, "node.crt"))

	stdout.Reset()
	stderr.Reset()
	if code := runCert([]string{"--dir", dir, "--advertise-host", "node2.example.com"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second runCert code=%d stderr=%s", code, stderr.String())
	}
	caAfter, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	nodeAfter, _ := os.ReadFile(filepath.Join(dir, "node.crt"))
	if !bytes.Equal(caBefore, caAfter) {
		t.Fatalf("ca.crt rotated on a re-issue; the CA must stay stable")
	}
	if bytes.Equal(nodeBefore, nodeAfter) {
		t.Fatalf("node.crt was not re-issued")
	}
	if !strings.Contains(stdout.String(), "existing devnet CA") {
		t.Fatalf("missing CA-reuse note: %s", stdout.String())
	}
}

func TestFaucetFlagsReachChildEnv(t *testing.T) {
	opts, code := parseOptions([]string{
		"--faucet",
		"--faucet-amount", "5",
		"--faucet-max", "25",
		"--faucet-cooldown", "1.5",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != -1 {
		t.Fatalf("parseOptions code=%d, want -1", code)
	}
	if !opts.faucet || opts.faucetAmount != "5" || opts.faucetMax != "25" || opts.faucetCooldown != "1.5" {
		t.Fatalf("faucet options: %+v", opts)
	}
	ports := nodePorts{API: 8000, P2P: 8765, Peer: 8770, Controller: 8769}
	env := envMap(buildChildEnv(opts, "/data", "/data/key.json", "ab", "0xabc", ports, true, "", nil))
	for name, want := range map[string]string{
		"SAN_FAUCET":          "1",
		"SAN_FAUCET_AMOUNT":   "5",
		"SAN_FAUCET_MAX":      "25",
		"SAN_FAUCET_COOLDOWN": "1.5",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestParseOptionsRejectsBadFaucetValues(t *testing.T) {
	cases := [][]string{
		{"--faucet-amount", "abc"},
		{"--faucet-max", "-1"},
		{"--faucet-cooldown", "soon"},
		{"--faucet-cooldown", "-2"},
	}
	for _, argv := range cases {
		if _, code := parseOptions(argv, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
			t.Errorf("parseOptions(%v) code=%d, want 2", argv, code)
		}
	}
}

func TestRunFaucetFlagParsing(t *testing.T) {
	cases := []struct {
		argv []string
		want int
	}{
		{[]string{}, 2},
		{[]string{"--to", "0xalice"}, 2},
		{[]string{"--to", "0x" + strings.Repeat("ab", 20), "--amount", "0"}, 2},
		{[]string{"--to", "0x" + strings.Repeat("ab", 20), "--amount", "1", "extra"}, 2},
	}
	for _, testCase := range cases {
		var stdout, stderr bytes.Buffer
		if code := runFaucet(testCase.argv, &stdout, &stderr); code != testCase.want {
			t.Errorf("runFaucet(%v) code=%d, want %d (%s)", testCase.argv, code, testCase.want, stderr.String())
		}
	}
}
