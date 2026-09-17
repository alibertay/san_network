// Package parity exposes the golden fixtures generated from the Python
// reference implementation (see tools/parity_fixtures.py). Go packages assert
// against these fixtures so the migration preserves exact behavior.
package parity

import (
	_ "embed"
)

//go:embed testdata/foundation.json
var foundation []byte

// Foundation returns the raw foundation fixture JSON.
func Foundation() []byte {
	return foundation
}
