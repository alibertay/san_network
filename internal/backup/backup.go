// Package backup implements section 23 backup, restore and validation for SAN
// node data directories. It copies the consensus database (LMDB), the node
// identity key and the peer cache atomically, records SHA-256 hashes in a
// manifest, and validates a restored database by reopening it and checking the
// canonical chain, the head state root and the persisted finality checkpoint.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/ledger/store"
)

const (
	// DefaultDBFile is the node database file inside a data directory
	// (buildChildEnv sets SAN_DB_PATH=<data-dir>/node.db).
	DefaultDBFile = "node.db"
	// DefaultKeyFile is the node identity key inside a data directory.
	DefaultKeyFile = "san_key.json"
	// DefaultPeerCacheFile is the rebuildable address-manager cache.
	DefaultPeerCacheFile = "peers-cache.json"
	// ManifestFile is the backup manifest written next to the copied files.
	ManifestFile = "manifest.json"
	// ManifestVersion is the backup format version.
	ManifestVersion = 1
)

// Manifest describes one backup set.
type Manifest struct {
	Version     int               `json:"version"`
	CreatedAt   string            `json:"created_at"`
	Backend     string            `json:"backend"`
	DBFile      string            `json:"db_file"`
	KeyFile     string            `json:"key_file"`
	Files       map[string]string `json:"files"` // base name -> sha256 hex
	Height      int64             `json:"height"`
	StateRoot   string            `json:"state_root"`
	Finalized   int64             `json:"finalized_height"`
	Consensus   []string          `json:"consensus_files"`
	Rebuildable []string          `json:"rebuildable_files"`
	Secrets     []string          `json:"secret_files"`
}

// Report is the result of validating a data directory.
type Report struct {
	Backend          string
	Height           int64
	StateRoot        string
	FinalizedHeight  int64
	FinalizedHash    string
	Blocks           int
	ChainValid       bool
	StateRootMatches bool
}

// Options selects the files to back up.
type Options struct {
	DataDir  string
	DBFile   string // absolute or relative to DataDir
	KeyFile  string // absolute or relative to DataDir
	Backend  string // "lmdb" or "memory"; empty defaults to lmdb
	PeerFile string // rebuildable peer cache, optional
}

// DefaultOptions builds options for a sanup-style data directory.
func DefaultOptions(dataDir string) Options {
	return Options{
		DataDir:  dataDir,
		DBFile:   filepath.Join(dataDir, DefaultDBFile),
		KeyFile:  filepath.Join(dataDir, DefaultKeyFile),
		PeerFile: filepath.Join(dataDir, DefaultPeerCacheFile),
		Backend:  "lmdb",
	}
}

// Backup copies the selected files into destDir atomically (staging directory
// then rename) and writes a manifest with SHA-256 hashes. destDir must not
// exist.
func Backup(options Options, destDir string) (*Manifest, error) {
	if strings.TrimSpace(options.DataDir) == "" {
		return nil, fmt.Errorf("backup: data directory is required")
	}
	if _, err := os.Stat(destDir); err == nil {
		return nil, fmt.Errorf("backup: destination %s already exists", destDir)
	}
	if options.DBFile == "" {
		options.DBFile = filepath.Join(options.DataDir, DefaultDBFile)
	}
	if options.KeyFile == "" {
		options.KeyFile = filepath.Join(options.DataDir, DefaultKeyFile)
	}
	if options.Backend == "" {
		options.Backend = "lmdb"
	}

	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("backup: create destination parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, filepath.Base(destDir)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("backup: create staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	manifest := &Manifest{
		Version:   ManifestVersion,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Backend:   options.Backend,
		DBFile:    filepath.Base(options.DBFile),
		KeyFile:   filepath.Base(options.KeyFile),
		Files:     map[string]string{},
		Consensus: []string{filepath.Base(options.DBFile)},
		Secrets:   []string{filepath.Base(options.KeyFile)},
	}

	// Consensus database (optional only when it does not exist yet).
	if err := copyInto(staging, options.DBFile, manifest, true); err != nil {
		cleanup()
		return nil, err
	}
	// Identity key: required for an existing node; a brand-new directory may
	// not have one yet.
	if err := copyInto(staging, options.KeyFile, manifest, true); err != nil {
		cleanup()
		return nil, err
	}
	// Rebuildable cache.
	if options.PeerFile != "" {
		if err := copyInto(staging, options.PeerFile, manifest, true); err != nil {
			cleanup()
			return nil, err
		}
		manifest.Rebuildable = append(manifest.Rebuildable, filepath.Base(options.PeerFile))
	}

	if _, err := os.Stat(options.DBFile); err == nil {
		height, root, finalized, validateErr := validateStore(options.DBFile, options.Backend)
		if validateErr != nil {
			cleanup()
			return nil, fmt.Errorf("backup: refusing to back up an invalid database: %w", validateErr)
		}
		manifest.Height = height
		manifest.StateRoot = root
		manifest.Finalized = finalized
	}

	encoded, err := canonical.Marshal(manifestToMap(manifest))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("backup: encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, ManifestFile), encoded, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("backup: write manifest: %w", err)
	}
	if err := os.Rename(staging, destDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("backup: publish backup: %w", err)
	}
	return manifest, nil
}

// copyInto copies source into the staging directory and records its hash.
// Missing optional files are skipped.
func copyInto(staging, source string, manifest *Manifest, optional bool) error {
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) && optional {
			return nil
		}
		return fmt.Errorf("backup: stat %s: %w", source, err)
	}
	if info.IsDir() {
		return fmt.Errorf("backup: %s is a directory; expected a file", source)
	}
	name := filepath.Base(source)
	digest, err := copyFile(source, filepath.Join(staging, name), info.Mode().Perm())
	if err != nil {
		return err
	}
	manifest.Files[name] = digest
	return nil
}

func copyFile(source, destination string, mode os.FileMode) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("backup: open %s: %w", source, err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return "", fmt.Errorf("backup: create %s: %w", destination, err)
	}
	hasher := sha256.New()
	writer := io.MultiWriter(output, hasher)
	if _, err := io.Copy(writer, input); err != nil {
		output.Close()
		return "", fmt.Errorf("backup: copy %s: %w", source, err)
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return "", fmt.Errorf("backup: sync %s: %w", destination, err)
	}
	if err := output.Close(); err != nil {
		return "", fmt.Errorf("backup: close %s: %w", destination, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// manifestToMap renders a manifest as a canonical-JSON object.
func manifestToMap(manifest *Manifest) map[string]any {
	files := map[string]any{}
	for name, digest := range manifest.Files {
		files[name] = digest
	}
	consensus := make([]any, len(manifest.Consensus))
	for index, name := range manifest.Consensus {
		consensus[index] = name
	}
	rebuildable := make([]any, len(manifest.Rebuildable))
	for index, name := range manifest.Rebuildable {
		rebuildable[index] = name
	}
	secrets := make([]any, len(manifest.Secrets))
	for index, name := range manifest.Secrets {
		secrets[index] = name
	}
	return map[string]any{
		"version":           manifest.Version,
		"created_at":        manifest.CreatedAt,
		"backend":           manifest.Backend,
		"db_file":           manifest.DBFile,
		"key_file":          manifest.KeyFile,
		"files":             files,
		"height":            manifest.Height,
		"state_root":        manifest.StateRoot,
		"finalized_height":  manifest.Finalized,
		"consensus_files":   consensus,
		"rebuildable_files": rebuildable,
		"secret_files":      secrets,
	}
}

// LoadManifest reads and decodes a backup manifest.
func LoadManifest(backupDir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(backupDir, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("restore: read manifest: %w", err)
	}
	decoded, err := canonical.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("restore: decode manifest: %w", err)
	}
	record, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("restore: manifest is not an object")
	}
	manifest := &Manifest{Files: map[string]string{}, Backend: "lmdb"}
	manifest.Version = int(toInt64(record["version"]))
	manifest.CreatedAt = stringValue(record["created_at"])
	manifest.Backend = stringValue(record["backend"])
	manifest.DBFile = stringValue(record["db_file"])
	manifest.KeyFile = stringValue(record["key_file"])
	manifest.Height = toInt64(record["height"])
	manifest.StateRoot = stringValue(record["state_root"])
	manifest.Finalized = toInt64(record["finalized_height"])
	if files, ok := record["files"].(map[string]any); ok {
		for name, digest := range files {
			manifest.Files[name] = stringValue(digest)
		}
	}
	if manifest.Version > ManifestVersion {
		return nil, fmt.Errorf("restore: backup manifest version %d is newer than this build (%d)",
			manifest.Version, ManifestVersion)
	}
	return manifest, nil
}

// Restore verifies every file hash and copies the backup into dataDir. dataDir
// must not exist (or must be an empty directory).
func Restore(backupDir, dataDir string) (*Manifest, error) {
	manifest, err := LoadManifest(backupDir)
	if err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(dataDir); err == nil {
		if len(entries) > 0 {
			return nil, fmt.Errorf("restore: destination %s is not empty", dataDir)
		}
	}
	parent := filepath.Dir(dataDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("restore: create destination parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, filepath.Base(dataDir)+".restore-*")
	if err != nil {
		return nil, fmt.Errorf("restore: create staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			cleanup()
			return nil, fmt.Errorf("restore: unsafe file name %q in manifest", name)
		}
		source := filepath.Join(backupDir, name)
		mode := os.FileMode(0o600)
		if info, err := os.Stat(source); err == nil {
			mode = info.Mode().Perm()
		}
		digest, err := copyFile(source, filepath.Join(staging, name), mode)
		if err != nil {
			cleanup()
			return nil, err
		}
		if expected := manifest.Files[name]; expected != "" && digest != expected {
			cleanup()
			return nil, fmt.Errorf("restore: hash mismatch for %s: got %s want %s", name, digest, expected)
		}
	}
	if err := os.Rename(staging, dataDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("restore: publish data directory: %w", err)
	}
	return manifest, nil
}

// ValidateDataDir opens the restored database and checks the canonical chain,
// the head state root and the persisted finality checkpoint.
func ValidateDataDir(dataDir, backend, dbFile string) (*Report, error) {
	if dbFile == "" {
		dbFile = filepath.Join(dataDir, DefaultDBFile)
	}
	if _, err := os.Stat(dbFile); err != nil {
		return nil, fmt.Errorf("validate: database %s: %w", dbFile, err)
	}
	_, _, _, err := validateStore(dbFile, backend)
	if err != nil {
		return nil, err
	}
	report, err := reportFor(dbFile, backend)
	if err != nil {
		return nil, err
	}
	return report, nil
}

// validateStore opens path with backend and runs the consistency checks.
func validateStore(path, backend string) (int64, string, int64, error) {
	keyValue, err := store.Open(path, backend)
	if err != nil {
		return 0, "", 0, err
	}
	defer keyValue.Close()
	chainStore, err := ledger.NewChainStore(path, backend, keyValue)
	if err != nil {
		return 0, "", 0, err
	}
	return validateChainStore(chainStore)
}

func validateChainStore(chainStore *ledger.ChainStore) (int64, string, int64, error) {
	highest, hasBlocks := chainStore.HighestHeight()
	if !hasBlocks {
		return 0, "", 0, nil
	}
	var previous string
	for height := int64(0); height <= highest; height++ {
		blockHash, ok := chainStore.BlockHashAt(height)
		if !ok {
			return 0, "", 0, fmt.Errorf("validate: canonical height %d has no block hash", height)
		}
		block, err := chainStore.BlockByHash(blockHash)
		if err != nil {
			return 0, "", 0, fmt.Errorf("validate: block %d: %w", height, err)
		}
		if block == nil {
			return 0, "", 0, fmt.Errorf("validate: canonical height %d has no block body", height)
		}
		if block.Index != height {
			return 0, "", 0, fmt.Errorf("validate: canonical height %d points at block index %d", height, block.Index)
		}
		if height > 0 && block.PreviousBlockHash != previous {
			return 0, "", 0, fmt.Errorf("validate: block %d does not chain from the previous hash", height)
		}
		previous = blockHash
	}

	head, err := chainStore.LoadBlock(highest)
	if err != nil || head == nil {
		return 0, "", 0, fmt.Errorf("validate: head block unreadable: %v", err)
	}
	stateRoot := ""
	state, err := chainStore.LoadState()
	if err != nil {
		return 0, "", 0, fmt.Errorf("validate: load state: %w", err)
	}
	if state != nil {
		storage, err := chainStore.LoadStorage()
		if err != nil {
			return 0, "", 0, fmt.Errorf("validate: load storage: %w", err)
		}
		stateRoot = ledger.StateRoot(stateInputFromSnapshot(state, storage))
		if rootText, ok := head.StateRoot.(string); ok && rootText != "" && stateRoot != rootText {
			return 0, "", 0, fmt.Errorf("validate: state root %s does not match head block %s", stateRoot, rootText)
		}
	}

	finalized := int64(0)
	if raw, ok := chainStore.GetMeta("finalized_height"); ok {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, "", 0, fmt.Errorf("validate: corrupt finalized_height %q", raw)
		}
		finalized = parsed
		if finalized > highest {
			return 0, "", 0, fmt.Errorf("validate: finalized height %d is beyond the tip %d", finalized, highest)
		}
		if hash, ok := chainStore.GetMeta("finalized_hash"); ok && hash != "" {
			if stored, ok := chainStore.BlockHashAt(finalized); ok && stored != hash {
				return 0, "", 0, fmt.Errorf("validate: finalized hash %s does not match canonical block %s at height %d",
					hash, stored, finalized)
			}
		}
	}
	return highest, stateRoot, finalized, nil
}

func reportFor(path, backend string) (*Report, error) {
	keyValue, err := store.Open(path, backend)
	if err != nil {
		return nil, err
	}
	defer keyValue.Close()
	chainStore, err := ledger.NewChainStore(path, backend, keyValue)
	if err != nil {
		return nil, err
	}
	highest, stateRoot, finalized, err := validateChainStore(chainStore)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Backend:          backend,
		Height:           highest,
		StateRoot:        stateRoot,
		FinalizedHeight:  finalized,
		ChainValid:       true,
		StateRootMatches: true,
	}
	if hash, ok := chainStore.GetMeta("finalized_hash"); ok {
		report.FinalizedHash = hash
	}
	for height := int64(0); height <= highest; height++ {
		if _, ok := chainStore.BlockHashAt(height); ok {
			report.Blocks++
		}
	}
	return report, nil
}

// ValidateStore runs the same checks against an already-open ChainStore (tests
// and embedders).
func ValidateStore(chainStore *ledger.ChainStore) (*Report, error) {
	highest, stateRoot, finalized, err := validateChainStore(chainStore)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Height:           highest,
		StateRoot:        stateRoot,
		FinalizedHeight:  finalized,
		ChainValid:       true,
		StateRootMatches: true,
		Blocks:           int(highest) + 1,
	}
	if hash, ok := chainStore.GetMeta("finalized_hash"); ok {
		report.FinalizedHash = hash
	}
	return report, nil
}

func stateInputFromSnapshot(state, storage map[string]any) ledger.StateInput {
	input := ledger.StateInput{
		Balances:   map[string]int64{},
		Nonces:     map[string]int64{},
		Validators: map[string]map[string]any{},
		Storage:    storage,
		Parameters: map[string]int64{},
	}
	if state == nil {
		return input
	}
	if raw, ok := state["balances"].(map[string]any); ok {
		for address, value := range raw {
			input.Balances[address] = toInt64(value)
		}
	}
	if raw, ok := state["nonces"].(map[string]any); ok {
		for address, value := range raw {
			input.Nonces[address] = toInt64(value)
		}
	}
	if raw, ok := state["validators"].(map[string]any); ok {
		for address, value := range raw {
			if record, ok := value.(map[string]any); ok {
				input.Validators[address] = record
			}
		}
	}
	if raw, ok := state["parameters"].(map[string]any); ok {
		for name, value := range raw {
			input.Parameters[name] = toInt64(value)
		}
	}
	input.TotalSlashed = toInt64(state["total_slashed"])
	input.TotalBurned = toInt64(state["total_burned"])
	input.BaseFee = toInt64(state["base_fee"])
	return input
}

func toInt64(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	default:
		return 0
	}
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}
