package ledger

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger/store"
)

// TestSchemaVersionFreshStore seeds the current schema marker on open.
func TestSchemaVersionFreshStore(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	chainStore, err := NewChainStore(":memory:", "memory", kv)
	if err != nil {
		t.Fatalf("NewChainStore: %v", err)
	}
	value, ok := chainStore.GetMeta("schema_version")
	if !ok {
		t.Fatalf("schema_version marker was not written")
	}
	if value != strconv.Itoa(StoreSchemaVersion) {
		t.Fatalf("schema_version = %q, want %d", value, StoreSchemaVersion)
	}
}

// TestSchemaVersionNewerRefused is the section 22 invariant: a database written
// by a newer build must fail loudly, never silently.
func TestSchemaVersionNewerRefused(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	if err := kv.Put([]byte(metaPrefix+"schema_version"), []byte(strconv.Itoa(StoreSchemaVersion+7))); err != nil {
		t.Fatalf("seed schema marker: %v", err)
	}
	_, err := NewChainStore(":memory:", "memory", kv)
	if err == nil {
		t.Fatalf("opening a newer-schema database succeeded")
	}
	var mismatch *store.SchemaMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error type %T is not SchemaMismatch: %v", err, err)
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("error does not explain the newer schema: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(StoreSchemaVersion+7)) {
		t.Fatalf("error does not mention the on-disk version: %v", err)
	}
}

// TestSchemaVersionInvalidRefused treats an unparsable marker as corruption.
func TestSchemaVersionInvalidRefused(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	if err := kv.Put([]byte(metaPrefix+"schema_version"), []byte("not-a-number")); err != nil {
		t.Fatalf("seed schema marker: %v", err)
	}
	_, err := NewChainStore(":memory:", "memory", kv)
	if err == nil {
		t.Fatalf("opening a corrupted schema marker succeeded")
	}
	var mismatch *store.SchemaMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error type %T is not SchemaMismatch: %v", err, err)
	}
}

// TestSchemaVersionOlderAccepted documents the supported upgrade path: an older
// schema opens in place and keeps its marker until a migration rewrites it.
func TestSchemaVersionOlderAccepted(t *testing.T) {
	kv := store.NewMemoryStore(":memory:")
	if err := kv.Put([]byte(metaPrefix+"schema_version"), []byte("0")); err != nil {
		t.Fatalf("seed schema marker: %v", err)
	}
	chainStore, err := NewChainStore(":memory:", "memory", kv)
	if err != nil {
		t.Fatalf("older schema must open in place: %v", err)
	}
	if value, _ := chainStore.GetMeta("schema_version"); value != "0" {
		t.Fatalf("older marker was silently rewritten to %q", value)
	}
}
