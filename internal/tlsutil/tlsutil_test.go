package tlsutil_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/tlsutil"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(readFile(t, path))
	if block == nil {
		t.Fatalf("%s is not PEM", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate(%s): %v", path, err)
	}
	return cert
}

// TestGenerateCAStableAcrossSignings verifies that signing node certificates
// never rotates the CA and that both node certificates chain to ca.crt.
func TestGenerateCAStableAcrossSignings(t *testing.T) {
	caDir := t.TempDir()
	caPaths, err := tlsutil.GenerateCA(caDir)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	caCertBefore := readFile(t, caPaths.CACert)
	caKeyBefore := readFile(t, caPaths.CAKey)
	ca := parseCert(t, caPaths.CACert)
	if !ca.IsCA {
		t.Fatalf("generated certificate is not a CA")
	}

	nodeADir := t.TempDir()
	pathsA, err := tlsutil.SignNodeCert(caDir, nodeADir, []string{"node1.example.com", "203.0.113.5"})
	if err != nil {
		t.Fatalf("SignNodeCert A: %v", err)
	}
	nodeBDir := t.TempDir()
	pathsB, err := tlsutil.SignNodeCert(caDir, nodeBDir, []string{"node2.example.com"})
	if err != nil {
		t.Fatalf("SignNodeCert B: %v", err)
	}

	if !bytes.Equal(caCertBefore, readFile(t, caPaths.CACert)) {
		t.Fatalf("ca.crt changed after signing node certificates")
	}
	if !bytes.Equal(caKeyBefore, readFile(t, caPaths.CAKey)) {
		t.Fatalf("ca.key changed after signing node certificates")
	}
	if parseCert(t, caPaths.CACert).SerialNumber.Cmp(ca.SerialNumber) != 0 {
		t.Fatalf("CA serial changed after signing node certificates")
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for name, path := range map[string]string{"A": pathsA.NodeCert, "B": pathsB.NodeCert} {
		leaf := parseCert(t, path)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: leaf.DNSNames[0]}); err != nil {
			t.Fatalf("node %s certificate does not verify against ca.crt: %v", name, err)
		}
		if leaf.IsCA {
			t.Fatalf("node %s certificate must not be a CA", name)
		}
	}

	leafA := parseCert(t, pathsA.NodeCert)
	if !strings.Contains(strings.Join(leafA.DNSNames, ","), "node1.example.com") {
		t.Fatalf("node A SANs missing node1.example.com: %v", leafA.DNSNames)
	}
	hasIP := false
	for _, ip := range leafA.IPAddresses {
		if ip.String() == "203.0.113.5" {
			hasIP = true
		}
	}
	if !hasIP {
		t.Fatalf("node A SANs missing 203.0.113.5: %v", leafA.IPAddresses)
	}
	if len(leafA.DNSNames) == 0 || !strings.Contains(strings.Join(leafA.DNSNames, ","), "localhost") {
		t.Fatalf("node A SANs missing the localhost fallback: %v", leafA.DNSNames)
	}

	if runtime.GOOS != "windows" {
		for _, pair := range [][2]string{
			{caPaths.CACert, "0644"},
			{caPaths.CAKey, "0600"},
			{pathsA.NodeCert, "0644"},
			{pathsA.NodeKey, "0600"},
			{pathsB.NodeKey, "0600"},
		} {
			info, err := os.Stat(pair[0])
			if err != nil {
				t.Fatalf("Stat(%s): %v", pair[0], err)
			}
			if got := fmt.Sprintf("%04o", info.Mode().Perm()); got != pair[1] {
				t.Errorf("%s permissions: got %s, want %s", filepath.Base(pair[0]), got, pair[1])
			}
		}
	}
}

// TestSignNodeCertRefusesMissingOrMismatchedCA covers the refusal paths.
func TestSignNodeCertRefusesMissingOrMismatchedCA(t *testing.T) {
	if _, err := tlsutil.SignNodeCert(t.TempDir(), t.TempDir(), nil); err == nil {
		t.Fatalf("SignNodeCert must refuse a directory without ca.crt/ca.key")
	}

	caDirA := t.TempDir()
	if _, err := tlsutil.GenerateCA(caDirA); err != nil {
		t.Fatalf("GenerateCA A: %v", err)
	}
	caDirB := t.TempDir()
	if _, err := tlsutil.GenerateCA(caDirB); err != nil {
		t.Fatalf("GenerateCA B: %v", err)
	}
	if err := os.Remove(filepath.Join(caDirA, "ca.key")); err != nil {
		t.Fatalf("Remove(ca.key): %v", err)
	}
	if err := os.WriteFile(filepath.Join(caDirA, "ca.key"), readFile(t, filepath.Join(caDirB, "ca.key")), 0o600); err != nil {
		t.Fatalf("WriteFile(ca.key): %v", err)
	}
	if _, err := tlsutil.SignNodeCert(caDirA, t.TempDir(), []string{"node1.example.com"}); err == nil {
		t.Fatalf("SignNodeCert must refuse a CA key that does not match ca.crt")
	}
}

// TestGenerateDevnetCertsLayout keeps the single-directory behavior used by
// `sanup cert` without flags.
func TestGenerateDevnetCertsLayout(t *testing.T) {
	dir := t.TempDir()
	paths, err := tlsutil.GenerateDevnetCerts(dir, []string{"node1.example.com"})
	if err != nil {
		t.Fatalf("GenerateDevnetCerts: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(paths.NodeCert, paths.NodeKey); err != nil {
		t.Fatalf("node key pair is unusable: %v", err)
	}
	if tlsutil.HasCA(dir) != true {
		t.Fatalf("HasCA(dir) must be true after GenerateDevnetCerts")
	}
	if tlsutil.HasCA(t.TempDir()) {
		t.Fatalf("HasCA(empty) must be false")
	}
}
