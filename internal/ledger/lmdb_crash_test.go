//go:build lmdb

package ledger

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger/store"
)

// TestLMDBProcessKillConsistency spawns a child process that keeps committing
// blocks into a real LMDB database and SIGKILLs it at a random moment, then
// reopens the database and asserts it is internally consistent (canonical
// chain valid, state root matches the head, indexes intact, no half block).
//
// The child is this same test binary re-executed with SAN_CRASH_CHILD=1; the
// parent needs no extra fixtures. Runs under `go test -tags lmdb` in WSL
// (cgo); skipped on hosts without LMDB.
func TestLMDBProcessKillConsistency(t *testing.T) {
	if os.Getenv("SAN_CRASH_CHILD") == "1" {
		runLMDBWriteLoop(os.Getenv("SAN_CRASH_DIR"))
		return
	}
	if _, err := store.OpenLMDB(filepath.Join(t.TempDir(), "probe.lmdb"), map[string]any{"map_size": int64(1 << 20)}); err != nil {
		t.Skipf("LMDB backend unavailable: %v", err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	generator := rand.New(rand.NewSource(20240913))
	for attempt := 0; attempt < 5; attempt++ {
		dir := t.TempDir()
		child := exec.Command(executable, "-test.run=^TestLMDBProcessKillConsistency$", "-test.v=false")
		child.Env = append(os.Environ(),
			"SAN_CRASH_CHILD=1",
			"SAN_CRASH_DIR="+dir,
		)
		child.Stdout = nil
		child.Stderr = nil
		if err := child.Start(); err != nil {
			t.Fatalf("start child: %v", err)
		}
		time.Sleep(time.Duration(150+generator.Intn(700)) * time.Millisecond)
		if err := child.Process.Kill(); err != nil {
			t.Fatalf("kill child: %v", err)
		}
		_ = child.Wait()

		dbPath := filepath.Join(dir, "chain.lmdb")
		kv, err := store.Open(dbPath, "lmdb")
		if err != nil {
			t.Fatalf("attempt %d: reopen after SIGKILL: %v", attempt, err)
		}
		chainStore, err := NewChainStore(dbPath, "lmdb", kv)
		if err != nil {
			t.Fatalf("attempt %d: NewChainStore after SIGKILL: %v", attempt, err)
		}
		assertCanonicalChain(t, chainStore)
		assertStateRootMatchesHead(t, chainStore)
		height, _ := chainStore.HighestHeight()
		t.Logf("attempt %d: reopened a consistent LMDB database at height %d", attempt, height)

		if _, ok := chainStore.BlockHashAt(height); ok {
			if block, err := chainStore.LoadBlock(height); err != nil || block == nil || block.Index != height {
				t.Fatalf("attempt %d: head block unreadable after SIGKILL: %v (%v)", attempt, block, err)
			}
		}
		if err := chainStore.Close(); err != nil {
			t.Fatalf("attempt %d: close: %v", attempt, err)
		}
	}
}

// runLMDBWriteLoop is the child: it commits valid blocks as fast as it can
// until the parent kills it. Write errors are fatal so the parent can tell a
// real failure from a kill.
func runLMDBWriteLoop(dir string) {
	dbPath := filepath.Join(dir, "chain.lmdb")
	kv, err := store.Open(dbPath, "lmdb")
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(3)
	}
	chainStore, err := NewChainStore(dbPath, "lmdb", kv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child chain store:", err)
		os.Exit(3)
	}
	previous := "0"
	for index := int64(0); ; index++ {
		block, state, storage := buildCrashBlock(index, previous)
		head := index
		receipts := []any{map[string]any{"tx_id": fmt.Sprintf("tx-%d", index), "tx_index": int64(0)}}
		if err := chainStore.AppendBlock(block, AppendOptions{
			Receipts: receipts, State: state, Storage: storage, Head: &head,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "child append:", err)
			os.Exit(3)
		}
		previous = block.CurrentBlockHash
	}
}

// TestLMDBNewerSchemaRefused fabricates a database written by a newer build and
// asserts the current build refuses to open it with a clear error.
func TestLMDBNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "newer.lmdb")
	kv, err := store.Open(dbPath, "lmdb")
	if err != nil {
		t.Skipf("LMDB backend unavailable: %v", err)
	}
	if err := kv.Put([]byte("m:schema_version"), []byte(strconv.Itoa(StoreSchemaVersion+1))); err != nil {
		t.Fatalf("seed schema marker: %v", err)
	}
	if err := kv.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	_, err = NewChainStore(dbPath, "lmdb", nil)
	if err == nil {
		t.Fatalf("opening a newer-schema LMDB database succeeded")
	}
	var mismatch *store.SchemaMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error type %T is not SchemaMismatch: %v", err, err)
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("error does not explain the newer schema: %v", err)
	}
}
