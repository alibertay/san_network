//go:build !lmdb

package store

import "fmt"

// OpenLMDB is available only in builds with the "lmdb" tag (requires cgo).
// The memory backend is always available; the LMDB backend is enabled in CI
// images that ship a C toolchain.
func OpenLMDB(path string, options ...any) (KeyValueStore, error) {
	return nil, &StorageError{Message: fmt.Sprintf(
		"cannot open the LMDB store %q: this build has no LMDB support (a C toolchain is required); "+
			"rebuild with `go build -tags lmdb` or set SAN_DB_BACKEND=memory",
		path,
	)}
}
