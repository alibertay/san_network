//go:build lmdb

package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// TestLMDBStoreRoundTrip exercises the cgo LMDB backend: reads, writes,
// atomic batches, prefix iteration with a start key, automatic map growth and
// persistence across close/reopen.
func TestLMDBStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.kv")
	options := map[string]any{"map_size": int64(MinimumLMDBMapSize), "sync": false}
	kv, err := OpenLMDB(path, options)
	if err != nil {
		t.Fatalf("OpenLMDB: %v", err)
	}
	if kv.Path() != path {
		t.Errorf("Path: got %q, want %q", kv.Path(), path)
	}

	if err := kv.Put([]byte("a:1"), []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := kv.Put([]byte("a:2"), []byte("two")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := kv.Put([]byte("b:1"), []byte("three")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if value, ok := kv.Get([]byte("a:1")); !ok || !bytes.Equal(value, []byte("one")) {
		t.Fatalf("Get(a:1): got %q ok=%v", value, ok)
	}
	if _, ok := kv.Get([]byte("missing")); ok {
		t.Fatalf("Get(missing) reported a value")
	}

	defaultPairs := kv.PrefixIterator([]byte("a:"), nil)
	if len(defaultPairs) != 2 || string(defaultPairs[0].Key) != "a:1" || string(defaultPairs[1].Key) != "a:2" {
		t.Fatalf("PrefixIterator(a:): got %v", defaultPairs)
	}
	startPairs := kv.PrefixIterator([]byte("a:"), []byte("a:2"))
	if len(startPairs) != 1 || string(startPairs[0].Key) != "a:2" {
		t.Fatalf("PrefixIterator(a:, from a:2): got %v", startPairs)
	}
	if outside := kv.PrefixIterator([]byte("a:"), []byte("b:0")); len(outside) != 0 {
		t.Fatalf("PrefixIterator(a:, from b:0): got %v", outside)
	}

	batch := &WriteBatch{}
	batch.Put([]byte("a:3"), []byte("four"))
	batch.Delete([]byte("a:1"))
	if err := kv.WriteBatch(batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if _, ok := kv.Get([]byte("a:1")); ok {
		t.Fatalf("deleted key still present")
	}
	if value, ok := kv.Get([]byte("a:3")); !ok || !bytes.Equal(value, []byte("four")) {
		t.Fatalf("Get(a:3): got %q ok=%v", value, ok)
	}
	// py-lmdb deletes missing keys without raising.
	if err := kv.Delete([]byte("does-not-exist")); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}

	// Fill more than the 1 MiB map in one batch: the backend must double the
	// map size once and retry (Python's _grow). LMDB page overhead is roughly
	// 2x the payload, so 800 x 1 KiB needs between 1 and 2 MiB.
	grow := &WriteBatch{}
	payload := bytes.Repeat([]byte("x"), 1024)
	for i := 0; i < 800; i++ {
		grow.Put([]byte(fmt.Sprintf("g:%04d", i)), payload)
	}
	if err := kv.WriteBatch(grow); err != nil {
		t.Fatalf("WriteBatch (map growth): %v", err)
	}
	if value, ok := kv.Get([]byte("g:0799")); !ok || len(value) != len(payload) {
		t.Fatalf("Get(g:0799) after growth: ok=%v len=%d", ok, len(value))
	}
	store, ok := kv.(*LMDBStore)
	if !ok {
		t.Fatalf("OpenLMDB did not return *LMDBStore")
	}
	if info, err := store.env.Info(); err != nil || info.MapSize <= MinimumLMDBMapSize {
		t.Fatalf("map did not grow: size=%v err=%v", info, err)
	}

	if err := kv.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := kv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenLMDB(path, options)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if value, ok := reopened.Get([]byte("a:2")); !ok || !bytes.Equal(value, []byte("two")) {
		t.Fatalf("Get(a:2) after reopen: got %q ok=%v", value, ok)
	}
	if _, ok := reopened.Get([]byte("a:1")); ok {
		t.Fatalf("deleted key reappeared after reopen")
	}
}
