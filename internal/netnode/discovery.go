package netnode

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local peer discovery: a small file-backed registry lets nodes on the same
// machine find each other without a --bootstrap argument. Every node publishes
// its own signed SelfPeerRecord (heartbeat refreshed while it runs) and reads
// the records other nodes publish. Writes are serialized with a lock file and
// applied with an atomic rename so concurrent starts cannot corrupt the file
// or lose entries, and reads ignore entries whose heartbeat is older than the
// configured TTL and whose signed record no longer verifies.
//
// Wide-area discovery (DNS seeds, --bootstrap seeds, gossip and the persisted
// address manager below) is the primary path for public devnets; the local
// registry is only a fallback for same-machine development.

const (
	// DefaultDiscoveryInterval is the registry refresh / read period.
	DefaultDiscoveryInterval = 5.0
	// DefaultDiscoveryTTL is how long an unrefreshed registry entry stays fresh.
	DefaultDiscoveryTTL = 600.0
	// registryVersion guards the on-disk schema.
	registryVersion = 1
	// discoveryProbeInterval bounds how often the local port range is scanned.
	discoveryProbeInterval = 30 * time.Second
	// discoveryAPIProbeFirst/Last and discoveryPeerProbeFirst/Last define the
	// fallback scan of the devnet defaults so nodes started by other means are
	// still found.
	discoveryAPIProbeFirst  = 8000
	discoveryAPIProbeLast   = 8010
	discoveryPeerProbeFirst = 8770
	discoveryPeerProbeLast  = 8780
	// dnsRefreshInterval bounds periodic DNS seed re-resolution.
	dnsRefreshInterval = 5 * time.Minute
)

// DefaultPeerRegistryPath returns SAN_PEER_REGISTRY when set, otherwise
// ~/.san/peers.json. An explicitly empty SAN_PEER_REGISTRY disables the local
// registry.
func DefaultPeerRegistryPath() string {
	if value, present := os.LookupEnv("SAN_PEER_REGISTRY"); present {
		return strings.TrimSpace(value)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".san", "peers.json")
}

// PeerRegistry is a JSON file shared by local nodes.
type PeerRegistry struct {
	Path string
	TTL  float64
}

// NewPeerRegistry builds a registry handle; no I/O happens until use.
func NewPeerRegistry(path string, ttl float64) *PeerRegistry {
	return &PeerRegistry{Path: path, TTL: ttl}
}

type registryEntry struct {
	Record    map[string]any `json:"record"`
	Heartbeat float64        `json:"heartbeat"`
}

type registryContents struct {
	Version   int             `json:"version"`
	UpdatedAt float64         `json:"updated_at"`
	Peers     []registryEntry `json:"peers"`
}

// LoadPeerRegistry returns the fresh signed records published in a registry
// file (best effort: a missing or unreadable file yields no peers).
func LoadPeerRegistry(path string, ttl float64) []map[string]any {
	return NewPeerRegistry(path, ttl).Fresh()
}

// RemovePeerRegistryEntry removes the entry matching publicKey (when set) or
// host:apiPort from the registry file. It is best effort.
func RemovePeerRegistryEntry(path, publicKey, host string, apiPort int64) error {
	return NewPeerRegistry(path, 0).Remove(publicKey, host, apiPort)
}

func (r *PeerRegistry) enabled() bool {
	return r != nil && strings.TrimSpace(r.Path) != ""
}

func (r *PeerRegistry) withLock(action func() error) error {
	if !r.enabled() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	lockPath := r.Path + ".lock"
	deadline := time.Now().Add(3 * time.Second)
	for {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = lock.Close()
			break
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 10*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("peer registry %s is locked by another process", r.Path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lockPath) }()
	return action()
}

func (r *PeerRegistry) loadLocked() registryContents {
	contents := registryContents{Version: registryVersion}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return contents
	}
	if err := json.Unmarshal(data, &contents); err != nil {
		return registryContents{Version: registryVersion}
	}
	if contents.Version == 0 {
		contents.Version = registryVersion
	}
	return contents
}

func (r *PeerRegistry) saveLocked(contents registryContents) error {
	contents.Version = registryVersion
	contents.UpdatedAt = nowSeconds()
	data, err := json.MarshalIndent(contents, "", "  ")
	if err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.tmp.%d", r.Path, os.Getpid())
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, r.Path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Chmod(r.Path, 0o600)
}

// sameRegistryOwner reports whether two records describe the same node.
func sameRegistryOwner(a, b map[string]any) bool {
	if a == nil || b == nil {
		return false
	}
	keyA, _ := a["public_key"].(string)
	keyB, _ := b["public_key"].(string)
	if keyA != "" && keyB != "" {
		return keyA == keyB
	}
	return stringValue(a["host"]) == stringValue(b["host"]) &&
		int64Value(a["api_port"]) == int64Value(b["api_port"])
}

// Upsert publishes (or refreshes) this node's signed record.
func (r *PeerRegistry) Upsert(record map[string]any) error {
	if !r.enabled() || record == nil {
		return nil
	}
	return r.withLock(func() error {
		contents := r.loadLocked()
		now := nowSeconds()
		replaced := false
		kept := contents.Peers[:0]
		for _, entry := range contents.Peers {
			if entry.Record == nil {
				continue
			}
			if sameRegistryOwner(entry.Record, record) {
				kept = append(kept, registryEntry{Record: record, Heartbeat: now})
				replaced = true
				continue
			}
			if r.TTL > 0 && now-entry.Heartbeat > r.TTL {
				continue
			}
			kept = append(kept, entry)
		}
		if !replaced {
			kept = append(kept, registryEntry{Record: record, Heartbeat: now})
		}
		contents.Peers = kept
		return r.saveLocked(contents)
	})
}

// Remove drops this node's record (publicKey when non-empty, otherwise
// host:apiPort) from the registry.
func (r *PeerRegistry) Remove(publicKey, host string, apiPort int64) error {
	if !r.enabled() {
		return nil
	}
	return r.withLock(func() error {
		contents := r.loadLocked()
		kept := make([]registryEntry, 0, len(contents.Peers))
		for _, entry := range contents.Peers {
			if entry.Record == nil {
				continue
			}
			match := false
			if publicKey != "" {
				recordKey, _ := entry.Record["public_key"].(string)
				match = recordKey == publicKey
			}
			if !match && host != "" {
				match = stringValue(entry.Record["host"]) == host &&
					int64Value(entry.Record["api_port"]) == apiPort
			}
			if !match {
				kept = append(kept, entry)
			}
		}
		if len(kept) == len(contents.Peers) {
			return nil
		}
		contents.Peers = kept
		return r.saveLocked(contents)
	})
}

// Fresh returns the records whose heartbeat is within the TTL.
func (r *PeerRegistry) Fresh() []map[string]any {
	if !r.enabled() {
		return nil
	}
	records := []map[string]any{}
	_ = r.withLock(func() error {
		contents := r.loadLocked()
		now := nowSeconds()
		for _, entry := range contents.Peers {
			if entry.Record == nil {
				continue
			}
			if r.TTL > 0 && now-entry.Heartbeat > r.TTL {
				continue
			}
			records = append(records, entry.Record)
		}
		return nil
	})
	return records
}

// ---------------------------------------------------------------------- #
// Node integration
// ---------------------------------------------------------------------- #

// discoveryActive reports whether the node should start the discovery and
// outbound maintenance loops: wide-area sources, the local registry, or a
// persisted address cache from an earlier run.
func (n *Node) discoveryActive() bool {
	if n.config.DiscoveryEnabled || n.config.WideAreaConfigured() {
		return true
	}
	path := n.config.ResolvedPeerCachePath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// startDiscovery seeds the address manager (persistent cache, bootstrap and
// DNS seeds), registers this node in the local registry when enabled and
// starts the discovery and outbound loops. It runs before the initial sync so
// discovered peers participate in bootstrap.
func (n *Node) startDiscovery(ctx context.Context) {
	wide := n.config.WideAreaConfigured()

	n.addrman = NewAddrManager(n.config.MaxAddrEntries, n.config.ResolvedPeerCachePath())
	n.addrman.SetValidator(n.VerifyPeerRecord)
	// The self filter must stay lock- and I/O-free: isSelf resolves the local
	// IP, so compare against the signed self record instead.
	selfRecord := n.SelfPeerRecord()
	n.addrman.SetSelfFilter(func(record map[string]any) bool { return sameRegistryOwner(record, selfRecord) })
	if loaded, err := n.addrman.Load(); err != nil {
		log.Printf("Cannot load the peer cache: %v", err)
	} else if loaded > 0 {
		log.Printf("Addrman: loaded %d cached address(es)", loaded)
	}
	for _, address := range n.config.BootstrapAddresses() {
		n.addSeedAddress(address, AddrSourceBootstrap)
	}
	n.resolveDNSSeeds(ctx)
	n.addrman.logStats()

	if n.config.DiscoveryEnabled {
		path := strings.TrimSpace(n.config.PeerRegistryPath)
		if path == "" {
			path = DefaultPeerRegistryPath()
		}
		if path != "" {
			n.registry = NewPeerRegistry(path, n.config.DiscoveryTTL)
			if err := n.registry.Upsert(n.SelfPeerRecord()); err != nil {
				log.Printf("Cannot register in the peer registry %s: %v", path, err)
			}
			if !wide {
				if added := n.addRecords(n.registry.Fresh(), AddrSourceRegistry); added > 0 {
					log.Printf("Discovery: %d peer(s) loaded from %s", added, path)
				}
			} else {
				log.Printf("Local peer registry %s is configured but wide-area discovery is active; registry records are ignored", path)
			}
		} else {
			log.Printf("Discovery is enabled but no peer registry path is available")
		}
	}

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.discoveryLoop(ctx)
	}()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.outboundLoop(ctx)
	}()
}

// addSeedAddress stores one seed/bootstrap address (host or host:port) in the
// address manager.
func (n *Node) addSeedAddress(raw, source string) bool {
	if n.addrman == nil {
		return false
	}
	host, port, ok := splitSeedAddress(raw, n.config.PeerPort)
	if !ok {
		log.Printf("Ignoring invalid seed address %q", raw)
		return false
	}
	record := map[string]any{
		"host":      host,
		"chain_id":  n.chainID,
		"api_port":  int64(n.config.APIPort),
		"p2p_port":  int64(n.config.P2PPort),
		"peer_port": int64(port),
		"tls":       n.config.TLSEnabled(),
	}
	return n.addrman.Add(record, source)
}

// resolveDNSSeeds resolves every configured DNS seed and feeds the addresses
// into the manager. It returns the number of new addresses.
func (n *Node) resolveDNSSeeds(ctx context.Context) int {
	if n.addrman == nil || len(n.config.DNSSeeds) == 0 {
		return 0
	}
	added := 0
	for _, seed := range n.config.DNSSeeds {
		addresses, err := resolveSeedHosts(ctx, seed, n.config.PeerPort)
		if err != nil {
			log.Printf("DNS seed %s failed: %v", seed, err)
			continue
		}
		for _, address := range addresses {
			if n.addSeedAddress(address, AddrSourceSeed) {
				added++
			}
		}
	}
	if added > 0 {
		log.Printf("DNS seeds provided %d new address(es)", added)
	}
	return added
}

// NoteInboundPeer records the remote host of an inbound gRPC session as a
// low-trust candidate; its ports come from our defaults and every dial still
// has to complete a signed handshake before it becomes a peer.
func (n *Node) NoteInboundPeer(address string) {
	if n.addrman == nil || strings.TrimSpace(address) == "" {
		return
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast()) {
		return
	}
	record := map[string]any{
		"host":      host,
		"chain_id":  n.chainID,
		"api_port":  int64(n.config.APIPort),
		"p2p_port":  int64(n.config.P2PPort),
		"peer_port": int64(n.config.PeerPort),
		"tls":       n.config.TLSEnabled(),
	}
	n.addrman.Add(record, AddrSourceInbound)
}

func recordsToAny(records []map[string]any) []any {
	values := make([]any, 0, len(records))
	for _, record := range records {
		values = append(values, record)
	}
	return values
}

func (n *Node) discoveryLoop(ctx context.Context) {
	interval := n.config.DiscoveryInterval
	if interval <= 0 {
		interval = DefaultDiscoveryInterval
	}
	ticker := time.NewTicker(time.Duration(interval * float64(time.Second)))
	defer ticker.Stop()
	lastProbe := time.Now()
	lastDNS := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		wide := n.config.WideAreaConfigured()
		added := 0
		if n.registry != nil {
			if err := n.registry.Upsert(n.SelfPeerRecord()); err != nil {
				log.Printf("Cannot refresh the peer registry entry: %v", err)
			}
			if !wide {
				added += n.addRecords(n.registry.Fresh(), AddrSourceRegistry)
			}
		}
		if len(n.config.DNSSeeds) > 0 && time.Since(lastDNS) >= dnsRefreshInterval {
			lastDNS = time.Now()
			added += n.resolveDNSSeeds(ctx)
		}
		if added > 0 {
			log.Printf("Discovery: %d new peer(s) learned", added)
			n.requestPeers(ctx)
			n.requestSync(ctx)
		}
		// The local port probe is a last resort for same-machine nodes that
		// were started without any discovery configuration; it never leaves
		// loopback.
		if n.config.DiscoveryProbe && !wide && time.Since(lastProbe) >= discoveryProbeInterval {
			lastProbe = time.Now()
			n.probeLocalPorts(ctx)
		}
	}
}

// requestSync runs at most one background chain sync at a time so a burst of
// newly discovered peers cannot pile up Synchronize calls.
func (n *Node) requestSync(ctx context.Context) {
	if !n.syncInFlight.CompareAndSwap(false, true) {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer n.syncInFlight.Store(false)
		n.Synchronize(ctx)
	}()
}

// probeLocalPorts scans the devnet default port range for nodes that were not
// started through the registry (e.g. a plain sannode) and merges whatever
// signed peer records their REST/bootstrap endpoints report.
func (n *Node) probeLocalPorts(ctx context.Context) {
	for port := discoveryAPIProbeFirst; port <= discoveryAPIProbeLast; port++ {
		if port == n.config.APIPort || !portOpen("127.0.0.1", port) {
			continue
		}
		peers := n.DiscoverPeers(fmt.Sprintf("127.0.0.1:%d", port))
		if len(peers) == 0 {
			continue
		}
		if added := n.AddPeers(peers); added > 0 {
			log.Printf("Discovery: %d peer(s) from local API port %d", added, port)
			n.requestPeers(ctx)
			n.requestSync(ctx)
		}
	}
	for port := discoveryPeerProbeFirst; port <= discoveryPeerProbeLast; port++ {
		if port == n.config.PeerPort || !portOpen("127.0.0.1", port) {
			continue
		}
		peers, err := RemoteBootstrap(ctx, n, fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err != nil || len(peers) == 0 {
			continue
		}
		if added := n.AddPeers(peers); added > 0 {
			log.Printf("Discovery: %d peer(s) from local peer port %d", added, port)
			n.requestPeers(ctx)
			n.requestSync(ctx)
		}
	}
}

func portOpen(host string, port int) bool {
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}
