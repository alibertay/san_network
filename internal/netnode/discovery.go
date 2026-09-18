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
)

// DefaultPeerRegistryPath returns SAN_PEER_REGISTRY when set, otherwise
// ~/.san/peers.json.
func DefaultPeerRegistryPath() string {
	if value := strings.TrimSpace(os.Getenv("SAN_PEER_REGISTRY")); value != "" {
		return value
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
	if err := os.Rename(temp, r.Path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
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

// startDiscovery registers this node, merges the current registry and starts
// the discovery loop. It is called from Node.Start before the initial sync so
// registry peers participate in bootstrap.
func (n *Node) startDiscovery(ctx context.Context) {
	path := strings.TrimSpace(n.config.PeerRegistryPath)
	if path == "" {
		path = DefaultPeerRegistryPath()
	}
	if path == "" {
		log.Printf("Discovery is enabled but no peer registry path is available")
		return
	}
	n.registry = NewPeerRegistry(path, n.config.DiscoveryTTL)
	if err := n.registry.Upsert(n.SelfPeerRecord()); err != nil {
		log.Printf("Cannot register in the peer registry %s: %v", path, err)
	}
	if added := n.AddPeers(recordsToAny(n.registry.Fresh())); added > 0 {
		log.Printf("Discovery: %d peer(s) loaded from %s", added, path)
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.discoveryLoop(ctx)
	}()
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if n.registry != nil {
			if err := n.registry.Upsert(n.SelfPeerRecord()); err != nil {
				log.Printf("Cannot refresh the peer registry entry: %v", err)
			}
			if added := n.AddPeers(recordsToAny(n.registry.Fresh())); added > 0 {
				log.Printf("Discovery: %d new peer(s) from %s", added, n.registry.Path)
				n.requestPeers(ctx)
				n.requestSync(ctx)
			}
		}
		if n.config.DiscoveryProbe && time.Since(lastProbe) >= discoveryProbeInterval {
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
