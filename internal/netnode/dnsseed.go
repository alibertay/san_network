package netnode

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DNS seeds are resolved with the system resolver (A and AAAA records); the
// resolved addresses feed the address manager. Tests replace dnsLookup.

const dnsSeedTimeout = 5 * time.Second

var dnsLookup = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// splitSeedAddress parses "host" or "host:port" (and bracketed IPv6);
// defaultPort is used when the entry carries no port.
func splitSeedAddress(raw string, defaultPort int) (string, int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, false
	}
	if host, portText, err := net.SplitHostPort(raw); err == nil {
		port, convErr := strconv.Atoi(portText)
		if convErr != nil || port <= 0 || port > 65535 {
			return "", 0, false
		}
		host = strings.Trim(host, "[]")
		if host == "" {
			return "", 0, false
		}
		return host, port, true
	}
	if ip := net.ParseIP(raw); ip != nil {
		return raw, defaultPort, true
	}
	if strings.Contains(raw, ":") {
		return "", 0, false
	}
	return raw, defaultPort, true
}

// resolveSeedHosts resolves one DNS seed entry into host:port candidates
// (deduplicated, sorted for determinism).
func resolveSeedHosts(ctx context.Context, seed string, defaultPort int) ([]string, error) {
	host, port, ok := splitSeedAddress(seed, defaultPort)
	if !ok {
		return nil, fmt.Errorf("invalid DNS seed %q", seed)
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{net.JoinHostPort(host, strconv.Itoa(port))}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, dnsSeedTimeout)
	defer cancel()
	addresses, err := dnsLookup(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve DNS seed %s: %w", host, err)
	}
	seen := map[string]bool{}
	results := []string{}
	for _, address := range addresses {
		candidate := net.JoinHostPort(address, strconv.Itoa(port))
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		results = append(results, candidate)
	}
	sort.Strings(results)
	return results, nil
}
