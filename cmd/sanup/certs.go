package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/alibertay/san_network/internal/tlsutil"
)

const certUsageText = `usage: sanup cert [options]

Generate a self-signed devnet CA and a node certificate (ECDSA P-256).

options:
  --dir DIR               output directory (default <data-dir>/certs)
  --data-dir DIR          data directory used for the default output path
  --advertise-host HOSTS  public DNS name(s)/IP(s) to include as SANs,
                          comma separated (the node's --advertise-host)
  --host HOSTS            additional SANs, comma separated

outputs:
  ca.crt / ca.key         trust anchor (distribute ca.crt only)
  node.crt / node.key     node certificate (node.key is 0600)

example:
  go run ./cmd/sanup cert --advertise-host node1.example.com
  go run ./cmd/sanup --tls-cert data/go-node/certs/node.crt \
      --tls-key data/go-node/certs/node.key \
      --tls-ca data/go-node/certs/ca.crt --advertise-host node1.example.com
`

func runCert(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sanup cert", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, certUsageText) }
	dir := flags.String("dir", "", "")
	dataDir := flags.String("data-dir", filepath.Join("data", "go-node"), "")
	advertiseHost := flags.String("advertise-host", "", "")
	extraHosts := flags.String("host", "", "")
	if err := flags.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "sanup cert: error: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}

	target := strings.TrimSpace(*dir)
	if target == "" {
		target = filepath.Join(*dataDir, "certs")
	}
	hosts := append(splitHosts(*advertiseHost), splitHosts(*extraHosts)...)
	paths, err := tlsutil.GenerateDevnetCerts(target, hosts)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot generate the certificates: %v\n", err)
		return 1
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		absolute = target
	}
	logf(stdout, "devnet certificates written to %s", absolute)
	fmt.Fprintf(stdout, "  CA   : %s   (copy this to every node; never copy ca.key)\n", paths.CACert)
	fmt.Fprintf(stdout, "  node : %s + %s (0600)\n", paths.NodeCert, paths.NodeKey)
	fmt.Fprintln(stdout, "Start this node with:")
	fmt.Fprintf(stdout, "  sanup --advertise-host HOST --tls-cert %s --tls-key %s --tls-ca %s\n",
		paths.NodeCert, paths.NodeKey, paths.CACert)
	fmt.Fprintln(stdout, "Every other node trusts the CA with:")
	fmt.Fprintf(stdout, "  SAN_TLS_CA=%s (copy ca.crt to that machine)\n", paths.CACert)
	return 0
}

func splitHosts(raw string) []string {
	hosts := []string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		hosts = append(hosts, entry)
	}
	return hosts
}
