package netnode

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Wide-area address manager, simplified from Bitcoin's addrman: a bounded,
// persisted set of candidate peers split into "new" (never dialled) and
// "tried" entries, with per-entry reconnect backoff and failure tracking.
//
// Entries from the local registry are never the primary source: seed,
// bootstrap, DNS and gossip records populate the manager when wide-area
// discovery is configured.

const (
	addrmanVersion = 1
	// maxAddrFailures evicts an address after this many consecutive failures.
	maxAddrFailures = 5
	// addrBackoffBase/Max bound the reconnect backoff (2^failures * base).
	addrBackoffBase = 5 * time.Second
	addrBackoffMax  = 15 * time.Minute
	// addrStaleTTL drops entries that were not seen for this long.
	addrStaleTTL = 30 * 24 * time.Hour

	// Address record sources.
	AddrSourceSeed      = "seed"
	AddrSourceBootstrap = "bootstrap"
	AddrSourceGossip    = "gossip"
	AddrSourceRegistry  = "registry"
	AddrSourceInbound   = "inbound"
)

type addrEntry struct {
	Record    map[string]any `json:"record"`
	Source    string         `json:"source"`
	FirstSeen float64        `json:"first_seen"`
	LastSeen  float64        `json:"last_seen"`
	LastTried float64        `json:"last_tried"`
	Failures  int            `json:"failures"`
	Tried     bool           `json:"tried"`
	// Score is the local peer reputation (peerscore.go); persisted so useful
	// reputation survives restarts. Local only: never consensus input.
	Score int64 `json:"score,omitempty"`
}

type addrCacheFile struct {
	Version int          `json:"version"`
	SavedAt float64      `json:"saved_at"`
	Entries []*addrEntry `json:"entries"`
}

// AddrManager is the persisted peer address book.
type AddrManager struct {
	mu         sync.Mutex
	path       string
	maxEntries int
	entries    map[string]*addrEntry
	validator  func(map[string]any) bool
	selfFilter func(map[string]any) bool
	now        func() float64
	dirty      bool
}

// NewAddrManager builds an empty manager; path may be empty to disable
// persistence, maxEntries <= 0 uses DefaultMaxAddrEntries.
func NewAddrManager(maxEntries int, path string) *AddrManager {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxAddrEntries
	}
	return &AddrManager{
		path:       strings.TrimSpace(path),
		maxEntries: maxEntries,
		entries:    map[string]*addrEntry{},
		now:        nowSeconds,
	}
}

// SetValidator installs the record verifier applied to gossiped records.
func (m *AddrManager) SetValidator(validator func(map[string]any) bool) {
	m.mu.Lock()
	m.validator = validator
	m.mu.Unlock()
}

// SetSelfFilter installs the local-record filter (never store ourselves).
func (m *AddrManager) SetSelfFilter(filter func(map[string]any) bool) {
	m.mu.Lock()
	m.selfFilter = filter
	m.mu.Unlock()
}

func (m *AddrManager) clock() float64 {
	if m.now != nil {
		return m.now()
	}
	return nowSeconds()
}

// AddrKey identifies an address by its dial endpoint (host:peer_port, falling
// back to p2p_port).
func AddrKey(record map[string]any) string {
	host := strings.ToLower(strings.TrimSpace(stringValue(record["host"])))
	port := int64Value(record["peer_port"])
	if port == 0 {
		port = int64Value(record["p2p_port"])
	}
	return host + ":" + fmt.Sprintf("%d", port)
}

// addrableRecord rejects records that cannot be dialled.
func addrableRecord(record map[string]any) bool {
	if record == nil {
		return false
	}
	host := strings.TrimSpace(stringValue(record["host"]))
	if host == "" || len(host) > 255 || strings.ContainsAny(host, " \t\r\n/\\") {
		return false
	}
	return true
}

// Add inserts or refreshes a candidate. Gossip-sourced records must pass the
// validator first. It returns true when a new address was stored.
func (m *AddrManager) Add(record map[string]any, source string) bool {
	if m == nil || !addrableRecord(record) {
		return false
	}
	if source == AddrSourceGossip {
		m.mu.Lock()
		validator := m.validator
		m.mu.Unlock()
		if validator != nil && !validator(record) {
			return false
		}
	}
	normalized := deepCopyStringMap(record)
	normalized["host"] = strings.TrimSpace(stringValue(normalized["host"]))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.selfFilter != nil && m.selfFilter(normalized) {
		return false
	}
	now := m.clock()
	key := AddrKey(normalized)
	if existing, ok := m.entries[key]; ok {
		if existing.Record == nil {
			existing.Record = normalized
		} else {
			for name, value := range normalized {
				existing.Record[name] = value
			}
		}
		existing.LastSeen = now
		if source != "" {
			existing.Source = source
		}
		m.dirty = true
		return false
	}
	for len(m.entries) >= m.maxEntries {
		if !m.evictLocked() {
			return false
		}
	}
	m.entries[key] = &addrEntry{
		Record:    normalized,
		Source:    source,
		FirstSeen: now,
		LastSeen:  now,
	}
	m.dirty = true
	return true
}

// evictLocked removes the least valuable entry: untried and least recently
// seen first, then the oldest tried entry.
func (m *AddrManager) evictLocked() bool {
	var victimKey string
	var victim *addrEntry
	for key, entry := range m.entries {
		if victim == nil ||
			(victim.Tried != entry.Tried && !entry.Tried) ||
			(victim.Tried == entry.Tried && entry.LastSeen < victim.LastSeen) ||
			(victim.Tried == entry.Tried && entry.LastSeen == victim.LastSeen && key < victimKey) {
			victimKey = key
			victim = entry
		}
	}
	if victim == nil {
		return false
	}
	delete(m.entries, victimKey)
	m.dirty = true
	return true
}

// MarkTried records a dial attempt so backoff starts immediately even when the
// attempt fails before MarkFailure runs.
func (m *AddrManager) MarkTried(record map[string]any) {
	m.update(record, func(entry *addrEntry, now float64) {
		entry.LastTried = now
		entry.Tried = true
	})
}

// MarkSuccess resets the failure counter after a completed handshake.
func (m *AddrManager) MarkSuccess(record map[string]any) {
	m.update(record, func(entry *addrEntry, now float64) {
		entry.LastSeen = now
		entry.LastTried = now
		entry.Failures = 0
		entry.Tried = true
	})
}

// SetScore persists the local reputation score of an address (local only).
func (m *AddrManager) SetScore(record map[string]any, score int64) {
	if m == nil || record == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[AddrKey(record)]
	if !ok {
		return
	}
	if entry.Score == score {
		return
	}
	entry.Score = score
	m.dirty = true
}

// MarkFailure increments the failure counter and returns true when the entry
// was evicted (maxAddrFailures reached).
func (m *AddrManager) MarkFailure(record map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[AddrKey(record)]
	if !ok {
		return false
	}
	entry.LastTried = m.clock()
	entry.Failures++
	m.dirty = true
	if entry.Failures >= maxAddrFailures {
		delete(m.entries, AddrKey(record))
		return true
	}
	return false
}

// Remove drops an address.
func (m *AddrManager) Remove(record map[string]any) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := AddrKey(record)
	if _, ok := m.entries[key]; !ok {
		return false
	}
	delete(m.entries, key)
	m.dirty = true
	return true
}

func (m *AddrManager) update(record map[string]any, action func(*addrEntry, float64)) {
	if m == nil || record == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[AddrKey(record)]
	if !ok {
		return
	}
	action(entry, m.clock())
	m.dirty = true
}

// Len returns the number of stored addresses.
func (m *AddrManager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Has reports whether the address is stored.
func (m *AddrManager) Has(record map[string]any) bool {
	if m == nil || record == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[AddrKey(record)]
	return ok
}

// backoffFor returns the reconnect delay after failures consecutive errors.
func backoffFor(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	delay := addrBackoffBase
	for i := 1; i < failures; i++ {
		delay *= 2
		if delay >= addrBackoffMax {
			return addrBackoffMax
		}
	}
	if delay > addrBackoffMax {
		return addrBackoffMax
	}
	return delay
}

// Select returns up to limit dialable addresses, preferring tried entries and
// skipping those still in backoff. The order is deterministic for a given
// clock so tests and restarts are reproducible.
func (m *AddrManager) Select(limit int) []map[string]any {
	if m == nil || limit <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	type candidate struct {
		key   string
		entry *addrEntry
	}
	tried := []candidate{}
	fresh := []candidate{}
	for key, entry := range m.entries {
		if entry.Record == nil || !addrableRecord(entry.Record) {
			continue
		}
		if now-entry.LastTried < backoffFor(entry.Failures).Seconds() {
			continue
		}
		item := candidate{key: key, entry: entry}
		if entry.Tried {
			tried = append(tried, item)
		} else {
			fresh = append(fresh, item)
		}
	}
	sort.Slice(tried, func(i, j int) bool {
		if tried[i].entry.Score != tried[j].entry.Score {
			return tried[i].entry.Score > tried[j].entry.Score
		}
		if tried[i].entry.LastTried != tried[j].entry.LastTried {
			return tried[i].entry.LastTried < tried[j].entry.LastTried
		}
		return tried[i].key < tried[j].key
	})
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].entry.LastSeen != fresh[j].entry.LastSeen {
			return fresh[i].entry.LastSeen > fresh[j].entry.LastSeen
		}
		return fresh[i].key < fresh[j].key
	})
	result := make([]map[string]any, 0, limit)
	triedIndex, freshIndex := 0, 0
	for len(result) < limit && (triedIndex < len(tried) || freshIndex < len(fresh)) {
		if triedIndex < len(tried) {
			result = append(result, deepCopyStringMap(tried[triedIndex].entry.Record))
			triedIndex++
			if len(result) >= limit {
				break
			}
		}
		if freshIndex < len(fresh) {
			result = append(result, deepCopyStringMap(fresh[freshIndex].entry.Record))
			freshIndex++
		}
	}
	return result
}

// Snapshot returns a copy of all stored records (debugging, tests).
func (m *AddrManager) Snapshot() []map[string]any {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.entries))
	for key := range m.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		records = append(records, deepCopyStringMap(m.entries[key].Record))
	}
	return records
}

// Dirty reports whether there are unsaved changes.
func (m *AddrManager) Dirty() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirty
}

// Save atomically persists the address book (0600).
func (m *AddrManager) Save() error {
	if m == nil || m.path == "" {
		return nil
	}
	m.mu.Lock()
	contents := addrCacheFile{Version: addrmanVersion, SavedAt: m.clock()}
	for _, entry := range m.entries {
		copied := *entry
		copied.Record = deepCopyStringMap(entry.Record)
		contents.Entries = append(contents.Entries, &copied)
	}
	m.dirty = false
	m.mu.Unlock()

	sort.Slice(contents.Entries, func(i, j int) bool {
		return AddrKey(contents.Entries[i].Record) < AddrKey(contents.Entries[j].Record)
	})
	data, err := json.MarshalIndent(contents, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.tmp.%d", m.path, os.Getpid())
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, m.path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	// An existing cache may have been created with wider permissions.
	return os.Chmod(m.path, 0o600)
}

// Load reads the persisted address book. Records are not re-validated against
// the peer-record TTL (signatures age out); the handshake still verifies every
// identity before a cached address can become a peer. Returns the number of
// loaded entries.
func (m *AddrManager) Load() (int, error) {
	if m == nil || m.path == "" {
		return 0, nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var contents addrCacheFile
	if err := json.Unmarshal(data, &contents); err != nil {
		return 0, fmt.Errorf("peer cache %s is corrupt: %w", m.path, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	loaded := 0
	for _, entry := range contents.Entries {
		if entry == nil || !addrableRecord(entry.Record) {
			continue
		}
		if m.selfFilter != nil && m.selfFilter(entry.Record) {
			continue
		}
		if entry.LastSeen > 0 && now-entry.LastSeen > addrStaleTTL.Seconds() {
			continue
		}
		key := AddrKey(entry.Record)
		if _, exists := m.entries[key]; exists {
			continue
		}
		if len(m.entries) >= m.maxEntries {
			break
		}
		copied := *entry
		m.entries[key] = &copied
		loaded++
	}
	return loaded, nil
}

// info exposes one entry's bookkeeping (tests).
func (m *AddrManager) info(record map[string]any) (addrEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[AddrKey(record)]
	if !ok {
		return addrEntry{}, false
	}
	return *entry, true
}

// LoadPeerCache reads a persisted address cache without applying record
// freshness rules (best effort; a missing or corrupt file yields no records).
// It is used by sanup to rediscover the genesis seed after a restart.
func LoadPeerCache(path string) []map[string]any {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var contents addrCacheFile
	if err := json.Unmarshal(data, &contents); err != nil {
		return nil
	}
	records := make([]map[string]any, 0, len(contents.Entries))
	for _, entry := range contents.Entries {
		if entry == nil || !addrableRecord(entry.Record) {
			continue
		}
		records = append(records, entry.Record)
	}
	return records
}

// logStats reports the address book size for the startup log.
func (m *AddrManager) logStats() {
	if m == nil {
		return
	}
	log.Printf("Addrman: %d address(es) tracked (cache %s)", m.Len(), m.path)
}
