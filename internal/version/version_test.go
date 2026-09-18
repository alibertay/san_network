package version

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveCarriesBuildMetadata(t *testing.T) {
	metadata := Resolve(2, 5)
	if metadata.ProtocolVersion != 2 {
		t.Errorf("protocol version = %d, want 2", metadata.ProtocolVersion)
	}
	if metadata.SchemaVersion != 5 {
		t.Errorf("schema version = %d, want 5", metadata.SchemaVersion)
	}
	if metadata.GoVersion == "" {
		t.Error("go version is empty")
	}
	if metadata.ProtocolName != ProtocolName {
		t.Errorf("protocol name = %q", metadata.ProtocolName)
	}
	if metadata.Version == "" {
		t.Error("version is empty")
	}
}

func TestMetadataStringAndJSON(t *testing.T) {
	metadata := Metadata{
		Version:         "1.2.3",
		Commit:          "abcdef0123456789abcdef0123456789abcdef01",
		BuildDate:       "2026-09-18T00:00:00Z",
		ProtocolName:    ProtocolName,
		ProtocolVersion: 2,
		SchemaVersion:   5,
		GoVersion:       "go1.26.0",
	}
	text := metadata.String()
	for _, expected := range []string{"san 1.2.3", "abcdef012345", "protocol san-p2p/2", "schema 5", "go1.26.0"} {
		if !strings.Contains(text, expected) {
			t.Errorf("String() = %q missing %q", text, expected)
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"version"`, `"commit"`, `"protocol_version"`, `"schema_version"`, `"go_version"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("JSON missing %s: %s", key, encoded)
		}
	}
}

func TestShortCommit(t *testing.T) {
	if got := ShortCommit("abcdef0123456789"); got != "abcdef012345" {
		t.Errorf("ShortCommit = %q", got)
	}
	if got := ShortCommit("abc"); got != "abc" {
		t.Errorf("ShortCommit short = %q", got)
	}
}
