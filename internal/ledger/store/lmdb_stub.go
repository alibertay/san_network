//go:build !lmdb

package store

// OpenLMDB is available only in builds with the "lmdb" tag (requires cgo).
// The memory backend is always available; the LMDB backend is enabled in CI
// images that ship a C toolchain.
func OpenLMDB(path string, options ...any) (KeyValueStore, error) {
	return nil, &StorageError{Message: "this build has no LMDB support; rebuild with -tags lmdb"}
}
