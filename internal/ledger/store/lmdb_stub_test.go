//go:build !lmdb

package store

import (
	"strings"
	"testing"
)

// TestOpenLMDBWithoutTagIsActionable: requesting SAN_DB_BACKEND=lmdb in a
// build without cgo/LMDB must fail with an error that names the database, the
// missing build tag and the memory fallback.
func TestOpenLMDBWithoutTagIsActionable(t *testing.T) {
	database := "data/node.db"
	_, err := Open(database, "lmdb")
	if err == nil {
		t.Fatalf("Open(lmdb) must fail in a non-cgo build")
	}
	message := err.Error()
	for _, want := range []string{database, "-tags lmdb", "SAN_DB_BACKEND=memory"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not mention %q", message, want)
		}
	}
}
