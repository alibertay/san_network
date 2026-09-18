package ledger

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSecretFileAndDirectoryPermissions pins the Linux posture for node keys:
// key files 0600, their directories 0700, and widening an existing file is
// corrected on the next save.
func TestSecretFileAndDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	path := filepath.Join(dir, "san_key.json")
	if err := identity.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file): %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file permissions: got %o, want 600", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("key directory permissions: got %o, want 700", got)
	}

	// An existing wider file is tightened, never left readable.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := identity.Save(path); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	fileInfo, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file) again: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file permissions after re-save: got %o, want 600", got)
	}
}
