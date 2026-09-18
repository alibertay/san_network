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

Generate a self-signed devnet CA and/or a node certificate (ECDSA P-256).

options:
  --dir DIR               output directory (default <data-dir>/certs)
  --data-dir DIR          data directory used for the default output path
  --advertise-host HOSTS  public DNS name(s)/IP(s) to include as SANs,
                          comma separated (the node's --advertise-host)
  --host HOSTS            additional SANs, comma separated
  --ca-dir DIR            sign with the CA in DIR instead of creating one
  --ca-only               create only the CA (ca.crt/ca.key), no node cert

behavior:
  --ca-only               writes ca.crt (0644) + ca.key (0600) into --dir and
                          leaves existing CA files untouched
  --ca-dir DIR            loads DIR/ca.crt + DIR/ca.key and writes node.crt +
                          node.key into --dir (the CA files are never touched)
  neither, CA present     reuses <dir>/ca.crt + <dir>/ca.key and only re-issues
                          the node certificate, so the CA does not rotate
  neither, no CA          creates a new CA plus the node certificate

distribution:
  node.crt + node.key     go ONLY to that one node
  ca.crt                  goes to EVERY node (SAN_TLS_CA)
  ca.key                  NEVER leaves the CA machine

example:
  # one-time on the CA machine
  sanup cert --ca-only --dir /etc/san/ca
  # per node (CA key stays on the CA machine)
  sanup cert --ca-dir /etc/san/ca --dir /etc/san/certs --advertise-host node1.example.com
  # single-directory devnet (new CA + node cert)
  sanup cert --advertise-host node1.example.com
`

func runCert(argv []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sanup cert", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, certUsageText) }
	dir := flags.String("dir", "", "")
	dataDir := flags.String("data-dir", filepath.Join("data", "go-node"), "")
	advertiseHost := flags.String("advertise-host", "", "")
	extraHosts := flags.String("host", "", "")
	caDir := flags.String("ca-dir", "", "")
	caOnly := flags.Bool("ca-only", false, "")
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
	caDirValue := strings.TrimSpace(*caDir)
	if *caOnly && caDirValue != "" {
		fmt.Fprintln(stderr, "sanup cert: error: --ca-only and --ca-dir are mutually exclusive")
		return 2
	}
	if *caOnly && (*advertiseHost != "" || *extraHosts != "") {
		fmt.Fprintln(stderr, "sanup cert: warning: --ca-only ignores --advertise-host/--host")
	}

	target := strings.TrimSpace(*dir)
	if target == "" {
		target = filepath.Join(*dataDir, "certs")
	}
	hosts := append(splitHosts(*advertiseHost), splitHosts(*extraHosts)...)
	absolute, err := filepath.Abs(target)
	if err != nil {
		absolute = target
	}

	if *caOnly {
		if tlsutil.HasCA(target) {
			paths := tlsutil.Paths{
				CACert: filepath.Join(target, "ca.crt"),
				CAKey:  filepath.Join(target, "ca.key"),
			}
			logf(stdout, "existing devnet CA kept in %s (ca.crt/ca.key untouched)", absolute)
			printCADistribution(stdout, paths, "")
			return 0
		}
		paths, err := tlsutil.GenerateCA(target)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] cannot generate the CA: %v\n", err)
			return 1
		}
		logf(stdout, "devnet CA written to %s", absolute)
		printCADistribution(stdout, paths, "")
		return 0
	}

	if caDirValue != "" {
		paths, err := tlsutil.SignNodeCert(caDirValue, target, hosts)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] cannot sign the node certificate: %v\n", err)
			return 1
		}
		logf(stdout, "node certificate written to %s (signed by the CA in %s)", absolute, caDirValue)
		printNodeDistribution(stdout, paths, true)
		return 0
	}

	if tlsutil.HasCA(target) {
		paths, err := tlsutil.SignNodeCert(target, target, hosts)
		if err != nil {
			fmt.Fprintf(stderr, "[sanup] cannot sign the node certificate: %v\n", err)
			return 1
		}
		logf(stdout, "existing devnet CA in %s kept; only the node certificate was re-issued", absolute)
		printNodeDistribution(stdout, paths, true)
		return 0
	}

	paths, err := tlsutil.GenerateDevnetCerts(target, hosts)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] cannot generate the certificates: %v\n", err)
		return 1
	}
	logf(stdout, "devnet certificates written to %s", absolute)
	printNodeDistribution(stdout, paths, true)
	return 0
}

// printNodeDistribution prints exactly where each file goes and how to start
// the node that owns the certificate.
func printNodeDistribution(stdout io.Writer, paths tlsutil.Paths, withCA bool) {
	fmt.Fprintf(stdout, "  node : %s + %s (0600) -- copy ONLY to this node\n", paths.NodeCert, paths.NodeKey)
	fmt.Fprintf(stdout, "  CA   : %s (0644) -- copy to EVERY node\n", paths.CACert)
	fmt.Fprintf(stdout, "  CA key: %s (0600) -- NEVER leaves the CA machine\n", paths.CAKey)
	fmt.Fprintln(stdout, "Start this node with:")
	fmt.Fprintf(stdout, "  sanup --advertise-host HOST --tls-cert %s --tls-key %s --tls-ca %s\n",
		paths.NodeCert, paths.NodeKey, paths.CACert)
	fmt.Fprintln(stdout, "Every other node trusts the CA with:")
	fmt.Fprintf(stdout, "  SAN_TLS_CA=%s (copy ca.crt to that machine)\n", paths.CACert)
	if withCA {
		fmt.Fprintln(stdout, "Re-issue another node certificate without rotating the CA:")
		fmt.Fprintf(stdout, "  sanup cert --ca-dir %s --dir OTHER_NODE_DIR --advertise-host OTHER_HOST\n",
			filepath.Dir(paths.CACert))
	}
}

// printCADistribution prints the CA-only distribution rules.
func printCADistribution(stdout io.Writer, paths tlsutil.Paths, nodeDir string) {
	fmt.Fprintf(stdout, "  CA    : %s (0644) -- copy to EVERY node\n", paths.CACert)
	fmt.Fprintf(stdout, "  CA key: %s (0600) -- NEVER leaves the CA machine\n", paths.CAKey)
	fmt.Fprintln(stdout, "Sign a node certificate (the CA key stays on this machine):")
	target := "NODE_DIR"
	if nodeDir != "" {
		target = nodeDir
	}
	fmt.Fprintf(stdout, "  sanup cert --ca-dir %s --dir %s --advertise-host HOST\n",
		filepath.Dir(paths.CAKey), target)
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
