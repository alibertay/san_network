package canonical

import (
	"strings"
	"testing"
)

// Surrogate / \U parity with Python (security review F13/F14) is a documented
// non-issue rather than an implemented behavior: Go strings are UTF-8 and
// cannot represent a lone UTF-16 surrogate, so Decode normalizes `"\ud800"`
// to U+FFFD while Python keeps it. Every string this protocol signs or hashes
// is ASCII (hex keys/hashes, addresses, chain ids, opcode/field names), so the
// difference is not reachable through protocol traffic. This test pins both
// halves of that statement.
func TestSurrogateEdgeDocumented(t *testing.T) {
	decoded, err := Decode([]byte(`"\ud800"`))
	if err != nil {
		t.Fatalf("Decode(lone surrogate): %v", err)
	}
	if decoded != "\uFFFD" {
		t.Fatalf("lone surrogate decoded to %q, expected U+FFFD", decoded)
	}
	encoded, err := Marshal(decoded)
	if err != nil {
		t.Fatalf("Marshal(U+FFFD): %v", err)
	}
	if string(encoded) != `"\ufffd"` {
		t.Fatalf("Marshal(U+FFFD) = %s", encoded)
	}

	// Python's json module rejects \U escapes (only \uXXXX is valid JSON), and
	// Go's encoding/json does the same, so no divergence is observable there.
	if _, err := Decode([]byte(`"\U00110000"`)); err == nil {
		t.Fatalf("Go must reject \\U escapes like Python's json.loads")
	}

	protocolStrings := []string{
		"san-devnet-1",
		"0x" + strings.Repeat("ab", 20),
		strings.Repeat("cd", 1312), // ML-DSA-44 public key hex
		"ML-DSA-44",
		"GET_PEERS",
		"set_param",
	}
	for _, sample := range protocolStrings {
		encoded, err := Marshal(sample)
		if err != nil {
			t.Fatalf("Marshal(%q): %v", sample, err)
		}
		roundTrip, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode(%q): %v", encoded, err)
		}
		if roundTrip != sample {
			t.Fatalf("round trip changed %q to %q", sample, roundTrip)
		}
	}
}
