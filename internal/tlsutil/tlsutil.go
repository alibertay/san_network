// Package tlsutil generates the self-signed devnet certificate authority and
// per-node certificates used to enable TLS on a public SAN devnet:
//
//	ca.crt / ca.key    (shared trust anchor, distribute ca.crt only)
//	node.crt / node.key (SANs: the advertised DNS/IP hosts + localhost)
//
// The node key is written 0600; the CA private key is 0600 as well.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths are the generated certificate and key files.
type Paths struct {
	CACert   string
	CAKey    string
	NodeCert string
	NodeKey  string
}

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	nodeValidity = 2 * 365 * 24 * time.Hour
)

// GenerateDevnetCerts creates (or overwrites) ca.crt, ca.key, node.crt and
// node.key in dir for the given DNS names / IP addresses. localhost and the
// loopback addresses are always included so health checks on the node work.
func GenerateDevnetCerts(dir string, hosts []string) (Paths, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Paths{}, fmt.Errorf("certificate directory is required")
	}
	paths := Paths{
		CACert:   filepath.Join(dir, "ca.crt"),
		CAKey:    filepath.Join(dir, "ca.key"),
		NodeCert: filepath.Join(dir, "node.crt"),
		NodeKey:  filepath.Join(dir, "node.key"),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Paths{}, err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Paths{}, err
	}
	caTemplate, err := template("SAN devnet CA", caValidity)
	if err != nil {
		return Paths{}, err
	}
	caTemplate.IsCA = true
	caTemplate.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
	caTemplate.BasicConstraintsValid = true
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return Paths{}, err
	}
	if err := writePEM(paths.CACert, "CERTIFICATE", caDER, 0o644); err != nil {
		return Paths{}, err
	}
	caKeyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return Paths{}, err
	}
	if err := writePEM(paths.CAKey, "EC PRIVATE KEY", caKeyDER, 0o600); err != nil {
		return Paths{}, err
	}

	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Paths{}, err
	}
	nodeTemplate, err := template("san-node", nodeValidity)
	if err != nil {
		return Paths{}, err
	}
	nodeTemplate.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	nodeTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	for _, host := range normalizeHosts(hosts) {
		if ip := net.ParseIP(host); ip != nil {
			nodeTemplate.IPAddresses = append(nodeTemplate.IPAddresses, ip)
		} else {
			nodeTemplate.DNSNames = append(nodeTemplate.DNSNames, host)
		}
	}
	nodeDER, err := x509.CreateCertificate(rand.Reader, nodeTemplate, caTemplate, &nodeKey.PublicKey, caKey)
	if err != nil {
		return Paths{}, err
	}
	if err := writePEM(paths.NodeCert, "CERTIFICATE", nodeDER, 0o644); err != nil {
		return Paths{}, err
	}
	nodeKeyDER, err := x509.MarshalECPrivateKey(nodeKey)
	if err != nil {
		return Paths{}, err
	}
	if err := writePEM(paths.NodeKey, "EC PRIVATE KEY", nodeKeyDER, 0o600); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func template(commonName string, validity time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"SAN Network devnet"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		BasicConstraintsValid: true,
	}, nil
}

// normalizeHosts deduplicates hosts and always includes localhost defaults.
func normalizeHosts(hosts []string) []string {
	seen := map[string]bool{}
	result := []string{}
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		result = append(result, host)
	}
	for _, host := range hosts {
		add(host)
	}
	for _, fallback := range []string{"localhost", "127.0.0.1", "::1"} {
		add(fallback)
	}
	return result
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if data == nil {
		return fmt.Errorf("cannot encode %s", path)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	// Overwriting an existing file keeps its old permissions, so tighten them
	// explicitly (the CA and node private keys must stay 0600).
	return os.Chmod(path, mode)
}
