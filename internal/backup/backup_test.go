package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

func writeTestFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, DefaultDBFile), "fake-lmdb-bytes", 0o600)
	writeTestFile(t, filepath.Join(source, DefaultKeyFile), `{"private_key":"secret"}`, 0o600)
	writeTestFile(t, filepath.Join(source, DefaultPeerCacheFile), `{"peers":[]}`, 0o644)

	options := DefaultOptions(source)
	options.Backend = "memory"
	backupDir := filepath.Join(t.TempDir(), "backup")
	manifest, err := Backup(options, backupDir)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if manifest.Version != ManifestVersion {
		t.Fatalf("manifest version %d, want %d", manifest.Version, ManifestVersion)
	}
	if manifest.Files[DefaultDBFile] == "" || manifest.Files[DefaultKeyFile] == "" {
		t.Fatalf("manifest is missing file hashes: %v", manifest.Files)
	}
	if len(manifest.Secrets) != 1 || manifest.Secrets[0] != DefaultKeyFile {
		t.Fatalf("manifest secret list: %v", manifest.Secrets)
	}
	if _, err := os.Stat(filepath.Join(backupDir, ManifestFile)); err != nil {
		t.Fatalf("manifest file: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(backupDir, restored); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readTestFile(t, filepath.Join(restored, DefaultDBFile)); got != "fake-lmdb-bytes" {
		t.Fatalf("restored db: %q", got)
	}
	if got := readTestFile(t, filepath.Join(restored, DefaultKeyFile)); got != `{"private_key":"secret"}` {
		t.Fatalf("restored key: %q", got)
	}
	info, err := os.Stat(filepath.Join(restored, DefaultKeyFile))
	if err != nil {
		t.Fatalf("stat restored key: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("restored key mode %o, want 600", info.Mode().Perm())
	}
}

func TestBackupRejectsExistingDestination(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, DefaultKeyFile), "{}", 0o600)
	destination := t.TempDir()
	options := DefaultOptions(source)
	options.Backend = "memory"
	if _, err := Backup(options, destination); err == nil {
		t.Fatalf("Backup overwrote an existing destination")
	}
}

func TestRestoreDetectsTampering(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, DefaultDBFile), "original", 0o600)
	writeTestFile(t, filepath.Join(source, DefaultKeyFile), "key", 0o600)
	options := DefaultOptions(source)
	options.Backend = "memory"
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(options, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	writeTestFile(t, filepath.Join(backupDir, DefaultDBFile), "tampered", 0o600)
	_, err := Restore(backupDir, filepath.Join(t.TempDir(), "restored"))
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered restore error = %v, want hash mismatch", err)
	}
}

func TestRestoreRejectsNewerManifest(t *testing.T) {
	backupDir := t.TempDir()
	manifest := `{"version":99,"files":{"a":"00"},"db_file":"a","key_file":"a"}`
	writeTestFile(t, filepath.Join(backupDir, ManifestFile), manifest, 0o600)
	_, err := Restore(backupDir, filepath.Join(t.TempDir(), "restored"))
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer manifest error = %v, want newer-version refusal", err)
	}
}

func TestRestoreRejectsUnsafeManifestPath(t *testing.T) {
	backupDir := t.TempDir()
	manifest := `{"version":1,"files":{"../escape":"00"}}`
	writeTestFile(t, filepath.Join(backupDir, ManifestFile), manifest, 0o600)
	_, err := Restore(backupDir, filepath.Join(t.TempDir(), "restored"))
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe path error = %v, want unsafe-file refusal", err)
	}
}

// buildBackupStore seeds a memory ChainStore with a consistent two-block chain.
func buildBackupStore(t *testing.T) *ledger.ChainStore {
	t.Helper()
	chainStore, err := ledger.NewChainStore(":memory:", "memory", store.NewMemoryStore(":memory:"))
	if err != nil {
		t.Fatalf("NewChainStore: %v", err)
	}
	state := map[string]any{
		"balances":      map[string]any{"0x" + "aa": int64(100)},
		"nonces":        map[string]any{},
		"validators":    map[string]any{},
		"total_slashed": int64(0),
		"total_burned":  int64(0),
		"base_fee":      int64(1),
		"parameters":    map[string]any{},
	}
	storage := map[string]any{"data": map[string]any{}, "functions": map[string]any{}, "contracts": map[string]any{}}
	previous := "0"
	for index := int64(0); index < 3; index++ {
		root := ledger.StateRoot(ledger.StateInput{
			Balances:   map[string]int64{"0x" + "aa": 100},
			Nonces:     map[string]int64{},
			Validators: map[string]map[string]any{},
			Storage:    storage,
			Parameters: map[string]int64{},
			BaseFee:    1,
		})
		block := ledger.NewBlock(index, previous, "validator", "signature", []any{},
			float64(index), "san-devnet-1", root, 0, nil)
		head := index
		receipts := []any{map[string]any{"tx_id": "tx", "tx_index": int64(0)}}
		if err := chainStore.AppendBlock(block, ledger.AppendOptions{
			Receipts: receipts, State: state, Storage: storage, Head: &head,
		}); err != nil {
			t.Fatalf("AppendBlock(%d): %v", index, err)
		}
		previous = block.CurrentBlockHash
	}
	return chainStore
}

func TestValidateStoreConsistentChain(t *testing.T) {
	chainStore := buildBackupStore(t)
	report, err := ValidateStore(chainStore)
	if err != nil {
		t.Fatalf("ValidateStore: %v", err)
	}
	if report.Height != 2 {
		t.Fatalf("height %d, want 2", report.Height)
	}
	if report.StateRoot == "" || !report.ChainValid || !report.StateRootMatches {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestValidateStoreDetectsStateRootMismatch(t *testing.T) {
	chainStore := buildBackupStore(t)
	if err := chainStore.KeyValue().Put([]byte("m:__state"),
		[]byte(`{"balances":{"0xaa":9999},"base_fee":1}`)); err != nil {
		t.Fatalf("tamper state: %v", err)
	}
	_, err := ValidateStore(chainStore)
	if err == nil || !strings.Contains(err.Error(), "state root") {
		t.Fatalf("tampered state error = %v, want state-root mismatch", err)
	}
}

func TestValidateStoreDetectsFinalityBeyondTip(t *testing.T) {
	chainStore := buildBackupStore(t)
	if err := chainStore.SetMeta("finalized_height", "99"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	_, err := ValidateStore(chainStore)
	if err == nil || !strings.Contains(err.Error(), "beyond the tip") {
		t.Fatalf("bad finality error = %v, want beyond-tip refusal", err)
	}
}
