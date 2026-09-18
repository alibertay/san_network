// Package store provides the key-value storage interface shared by all
// backends, mirroring blockchain/storage in the Python implementation.
package store

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
)

// StorageError is the base class for storage failures.
type StorageError struct{ Message string }

func (e *StorageError) Error() string { return e.Message }

// SchemaMismatch marks an on-disk schema newer than this build understands.
type SchemaMismatch struct{ StorageError }

type operation struct {
	key    []byte
	value  []byte // nil means delete
	delete bool
}

// WriteBatch is a group of writes that must be applied atomically.
type WriteBatch struct {
	ops []operation
}

// Put stages a key/value write.
func (batch *WriteBatch) Put(key, value []byte) {
	batch.ops = append(batch.ops, operation{key: key, value: value})
}

// Delete stages a key removal.
func (batch *WriteBatch) Delete(key []byte) {
	batch.ops = append(batch.ops, operation{key: key, delete: true})
}

// Len returns the number of staged operations.
func (batch *WriteBatch) Len() int { return len(batch.ops) }

// Op is a read-only view of one staged batch operation (tests, migration
// tooling and fault-injecting wrappers).
type Op struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// ExportOps returns a copy of the staged operations in order. The slices share
// the batch's backing arrays; callers must not mutate them.
func (batch *WriteBatch) ExportOps() []Op {
	ops := make([]Op, len(batch.ops))
	for index, staged := range batch.ops {
		ops[index] = Op{Key: staged.key, Value: staged.value, Delete: staged.delete}
	}
	return ops
}

// Pair is a key/value entry returned by iterators.
type Pair struct {
	Key   []byte
	Value []byte
}

// KeyValueStore is a minimal ordered key-value store with atomic batches.
type KeyValueStore interface {
	Path() string
	Get(key []byte) ([]byte, bool)
	Put(key, value []byte) error
	Delete(key []byte) error
	WriteBatch(batch *WriteBatch) error
	// PrefixIterator iterates (key, value) pairs whose key starts with prefix;
	// start (optional) begins the iteration at that full key.
	PrefixIterator(prefix, start []byte) []Pair
	Close() error
	Flush() error
}

// MemoryStore is an in-memory store (tests, ephemeral nodes).
type MemoryStore struct {
	path string
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore(path string) *MemoryStore {
	if path == "" {
		path = ":memory:"
	}
	return &MemoryStore{path: path, data: map[string][]byte{}}
}

// Path returns the store path.
func (s *MemoryStore) Path() string { return s.path }

// Get returns the value for key.
func (s *MemoryStore) Get(key []byte) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.data[string(key)]
	if !ok {
		return nil, false
	}
	return append([]byte{}, value...), true
}

// Put stores a value.
func (s *MemoryStore) Put(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[string(key)] = append([]byte{}, value...)
	return nil
}

// Delete removes a key.
func (s *MemoryStore) Delete(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, string(key))
	return nil
}

// WriteBatch applies every operation or none of them.
func (s *MemoryStore) WriteBatch(batch *WriteBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range batch.ops {
		if op.delete {
			delete(s.data, string(op.key))
			continue
		}
		s.data[string(op.key)] = append([]byte{}, op.value...)
	}
	return nil
}

// PrefixIterator returns matching pairs sorted by key.
func (s *MemoryStore) PrefixIterator(prefix, start []byte) []Pair {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for key := range s.data {
		keyBytes := []byte(key)
		if !bytes.HasPrefix(keyBytes, prefix) {
			continue
		}
		if start != nil && bytes.Compare(keyBytes, start) < 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]Pair, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, Pair{Key: []byte(key), Value: append([]byte{}, s.data[key]...)})
	}
	return pairs
}

// Close clears the store.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = map[string][]byte{}
	return nil
}

// Flush is a no-op for the memory store.
func (s *MemoryStore) Flush() error { return nil }

// Snapshot returns a copy of every stored pair (parity tests, debugging).
func (s *MemoryStore) Snapshot() map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]byte, len(s.data))
	for key, value := range s.data {
		result[key] = append([]byte{}, value...)
	}
	return result
}

// Open selects a backend by name ("memory" or "lmdb").
func Open(path, backend string, options ...any) (KeyValueStore, error) {
	switch backend {
	case "", "memory":
		return NewMemoryStore(path), nil
	case "lmdb":
		return OpenLMDB(path, options...)
	default:
		return nil, fmt.Errorf("unknown storage backend: %q", backend)
	}
}
