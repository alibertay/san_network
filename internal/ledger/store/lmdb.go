//go:build lmdb

// LMDB backend, ported from blockchain/storage/lmdb_store.py and built with
// cgo. Build with "go build -tags lmdb"; without the tag the stub in
// lmdb_stub.go is compiled instead and OpenLMDB fails cleanly.
package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/PowerDNS/lmdb-go/lmdb"
)

// LMDB defaults mirror blockchain/storage/lmdb_store.py.
const (
	DefaultLMDBMapSize    = int64(1) << 30 // 1 GiB, grown automatically when full
	MinimumLMDBMapSize    = int64(1) << 20 // 1 MiB
	DefaultLMDBMaxReaders = 126
)

// LMDBStore is an LMDB-backed KeyValueStore. Like the Python implementation it
// opens a single file (subdir=false / NoSubdir), uses the unnamed root
// database, applies WriteBatch atomically and doubles the map size when full.
type LMDBStore struct {
	path    string
	env     *lmdb.Env
	mu      sync.Mutex // serialises writers, map growth and close
	mapSize int64
}

var _ KeyValueStore = (*LMDBStore)(nil)

type lmdbSettings struct {
	mapSize    int64
	maxReaders int
	sync       bool
}

// OpenLMDB opens (or creates) an LMDB database at path. Optional settings can
// be passed as a map[string]any with the keys "map_size", "max_readers" and
// "sync" (matching the Python constructor keywords), or as a bare integer map
// size. The defaults match blockchain/storage/lmdb_store.py.
func OpenLMDB(path string, options ...any) (KeyValueStore, error) {
	settings, err := parseLMDBOptions(options)
	if err != nil {
		return nil, err
	}
	if directory := filepath.Dir(path); directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, &StorageError{Message: fmt.Sprintf("Could not create LMDB directory %s: %v", directory, err)}
		}
	}

	env, err := lmdb.NewEnv()
	if err != nil {
		return nil, &StorageError{Message: fmt.Sprintf("Could not open LMDB store %s: %v", path, err)}
	}
	fail := func(err error) (KeyValueStore, error) {
		_ = env.Close()
		return nil, err
	}
	if err := env.SetMaxReaders(settings.maxReaders); err != nil {
		return fail(&StorageError{Message: fmt.Sprintf("Could not open LMDB store %s: %v", path, err)})
	}
	if err := env.SetMapSize(settings.mapSize); err != nil {
		return fail(&StorageError{Message: fmt.Sprintf("Could not open LMDB store %s: %v", path, err)})
	}
	// Python passes subdir=False, sync=sync and metasync=sync; both are false
	// by default, which maps onto the NoSync/NoMetaSync flags.
	flags := uint(lmdb.NoSubdir)
	if !settings.sync {
		flags |= lmdb.NoSync | lmdb.NoMetaSync
	}
	if err := env.Open(path, flags, 0o644); err != nil {
		return fail(&StorageError{Message: fmt.Sprintf("Could not open LMDB store %s: %v", path, err)})
	}
	return &LMDBStore{path: path, env: env, mapSize: settings.mapSize}, nil
}

func parseLMDBOptions(options []any) (lmdbSettings, error) {
	settings := lmdbSettings{mapSize: DefaultLMDBMapSize, maxReaders: DefaultLMDBMaxReaders}
	for _, option := range options {
		switch value := option.(type) {
		case nil:
		case int:
			settings.mapSize = int64(value)
		case int64:
			settings.mapSize = value
		case float64:
			settings.mapSize = int64(value)
		case map[string]any:
			for key, item := range value {
				switch key {
				case "map_size":
					size, ok := lmdbOptionInt(item)
					if !ok {
						return settings, &StorageError{Message: fmt.Sprintf("invalid LMDB map_size: %v", item)}
					}
					settings.mapSize = size
				case "max_readers":
					count, ok := lmdbOptionInt(item)
					if !ok {
						return settings, &StorageError{Message: fmt.Sprintf("invalid LMDB max_readers: %v", item)}
					}
					settings.maxReaders = int(count)
				case "sync":
					flag, ok := item.(bool)
					if !ok {
						return settings, &StorageError{Message: fmt.Sprintf("invalid LMDB sync: %v", item)}
					}
					settings.sync = flag
				default:
					return settings, &StorageError{Message: fmt.Sprintf("unknown LMDB option: %q", key)}
				}
			}
		default:
			return settings, &StorageError{Message: fmt.Sprintf("unsupported LMDB option type: %T", option)}
		}
	}
	if settings.mapSize < MinimumLMDBMapSize {
		settings.mapSize = MinimumLMDBMapSize
	}
	if settings.maxReaders < 1 {
		settings.maxReaders = DefaultLMDBMaxReaders
	}
	return settings, nil
}

func lmdbOptionInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float64:
		return int64(typed), true
	default:
		return 0, false
	}
}

// Path returns the store path.
func (s *LMDBStore) Path() string { return s.path }

// Get returns the value for key.
func (s *LMDBStore) Get(key []byte) ([]byte, bool) {
	var result []byte
	err := s.env.View(func(txn *lmdb.Txn) error {
		dbi, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		value, err := txn.Get(dbi, key)
		if lmdb.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		result = append([]byte{}, value...)
		return nil
	})
	if err != nil {
		return nil, false
	}
	return result, result != nil
}

// Put stores a value.
func (s *LMDBStore) Put(key, value []byte) error {
	batch := &WriteBatch{}
	batch.Put(key, value)
	return s.WriteBatch(batch)
}

// Delete removes a key.
func (s *LMDBStore) Delete(key []byte) error {
	batch := &WriteBatch{}
	batch.Delete(key)
	return s.WriteBatch(batch)
}

// WriteBatch applies every operation or none of them. When the map is full it
// is doubled and the batch retried once, exactly like Python's write_batch.
func (s *LMDBStore) WriteBatch(batch *WriteBatch) error {
	if batch.Len() == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.apply(batch)
	if err != nil && lmdb.IsMapFull(err) {
		if growErr := s.grow(); growErr != nil {
			return growErr
		}
		err = s.apply(batch)
	}
	if err != nil {
		return &StorageError{Message: fmt.Sprintf("LMDB write batch failed: %v", err)}
	}
	return nil
}

func (s *LMDBStore) apply(batch *WriteBatch) error {
	return s.env.Update(func(txn *lmdb.Txn) error {
		dbi, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		for _, op := range batch.ops {
			if op.delete {
				// py-lmdb's delete returns False for a missing key; unlike
				// mdb_del it does not raise, so NotFound is not an error.
				if err := txn.Del(dbi, op.key, nil); err != nil && !lmdb.IsNotFound(err) {
					return err
				}
				continue
			}
			if err := txn.Put(dbi, op.key, op.value, 0); err != nil {
				return err
			}
		}
		return nil
	})
}

// grow doubles the map size (Python's _grow). The caller holds s.mu.
func (s *LMDBStore) grow() error {
	newSize := s.mapSize * 2
	if newSize <= s.mapSize {
		return &StorageError{Message: "Could not grow LMDB map: map size overflow"}
	}
	if err := s.env.SetMapSize(newSize); err != nil {
		return &StorageError{Message: fmt.Sprintf("Could not grow LMDB map: %v", err)}
	}
	s.mapSize = newSize
	return nil
}

// PrefixIterator returns the pairs whose key starts with prefix, in key order,
// beginning at start when one is given (inclusive, like cursor.set_range).
func (s *LMDBStore) PrefixIterator(prefix, start []byte) []Pair {
	begin := prefix
	if len(start) > 0 {
		begin = start
	}
	pairs := []Pair{}
	err := s.env.View(func(txn *lmdb.Txn) error {
		dbi, err := txn.OpenRoot(0)
		if err != nil {
			return err
		}
		cursor, err := txn.OpenCursor(dbi)
		if err != nil {
			return err
		}
		defer cursor.Close()

		key, value, err := cursor.Get(begin, nil, lmdb.SetRange)
		if lmdb.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for bytes.HasPrefix(key, prefix) {
			pairs = append(pairs, Pair{
				Key:   append([]byte{}, key...),
				Value: append([]byte{}, value...),
			})
			key, value, err = cursor.Get(nil, nil, lmdb.Next)
			if lmdb.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return []Pair{}
	}
	return pairs
}

// Flush mirrors Python's flush(), which calls env.sync() with py-lmdb's
// default force=False.
func (s *LMDBStore) Flush() error {
	if err := s.env.Sync(false); err != nil {
		return &StorageError{Message: fmt.Sprintf("Could not flush LMDB store: %v", err)}
	}
	return nil
}

// Close syncs (best effort, like Python) and closes the environment.
func (s *LMDBStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.env.Sync(false)
	if err := s.env.Close(); err != nil {
		return &StorageError{Message: fmt.Sprintf("Could not close LMDB store: %v", err)}
	}
	return nil
}
