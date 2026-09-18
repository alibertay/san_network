package sanlog

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

func restoreLogger(t *testing.T) {
	t.Helper()
	previous := log.Writer()
	previousFlags := log.Flags()
	t.Cleanup(func() {
		log.SetOutput(previous)
		log.SetFlags(previousFlags)
	})
}

func TestTextFormatKeepsMessageAndFields(t *testing.T) {
	restoreLogger(t)
	var buffer bytes.Buffer
	if err := Configure(Config{Format: "text", Level: LevelInfo, Output: &buffer}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	Structured(LevelInfo, "block finalized", Fields("height", 7, "block_hash", "abc"))
	log.Print("plain line")

	output := buffer.String()
	if !strings.Contains(output, "block finalized") ||
		!strings.Contains(output, "height=7") ||
		!strings.Contains(output, "block_hash=abc") {
		t.Errorf("structured text missing message or fields: %q", output)
	}
	if !strings.Contains(output, "plain line") {
		t.Errorf("plain line missing: %q", output)
	}
	if strings.Contains(output, "[INFO]") {
		t.Errorf("level prefix should be stripped from text output: %q", output)
	}
}

func TestJSONFormatEmitsStructuredRecord(t *testing.T) {
	restoreLogger(t)
	var buffer bytes.Buffer
	if err := Configure(Config{Format: "json", Level: LevelInfo, Output: &buffer, Timestamps: true}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	Structured(LevelWarn, "handshake rejected", Fields("peer", "1.2.3.4:8770", "error_type", "protocol"))
	log.Print("ordinary message")

	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON records, got %d: %q", len(lines), buffer.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("record is not JSON: %v (%q)", err, lines[0])
	}
	if record["level"] != "warn" || record["msg"] != "handshake rejected" {
		t.Errorf("unexpected record: %v", record)
	}
	if record["peer"] != "1.2.3.4:8770" || record["error_type"] != "protocol" {
		t.Errorf("fields missing from record: %v", record)
	}
	if _, ok := record["ts"].(string); !ok {
		t.Errorf("timestamp missing: %v", record)
	}
	if err := json.Unmarshal([]byte(lines[1]), &record); err != nil {
		t.Fatalf("second record is not JSON: %v", err)
	}
	if record["level"] != "info" || record["msg"] != "ordinary message" {
		t.Errorf("unexpected second record: %v", record)
	}
}

func TestLevelFiltering(t *testing.T) {
	restoreLogger(t)
	var buffer bytes.Buffer
	if err := Configure(Config{Format: "text", Level: LevelWarn, Output: &buffer}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	Structured(LevelDebug, "debug line", nil)
	Structured(LevelInfo, "info line", nil)
	Structured(LevelWarn, "warn line", nil)
	Structured(LevelError, "error line", nil)

	output := buffer.String()
	if strings.Contains(output, "debug line") || strings.Contains(output, "info line") {
		t.Errorf("level filter leaked verbose lines: %q", output)
	}
	if !strings.Contains(output, "warn line") || !strings.Contains(output, "error line") {
		t.Errorf("level filter dropped important lines: %q", output)
	}
}

func TestRedactionCoversCredentials(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  string
	}{
		{"json private key", `{"private_key": "abcdef0123456789", "chain_id": "san-devnet-1"}`, "abcdef0123456789"},
		{"api token", `api_token=super-secret-value rest`, "super-secret-value"},
		{"bearer", `Authorization: Bearer tok_1234567890`, "tok_1234567890"},
		{"query token", `GET /health?token=abc123def HTTP/1.1`, "abc123def"},
		{"hex secret key", `secret_key: ` + strings.Repeat("ab", 40), strings.Repeat("ab", 40)},
		{"pem", "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADAN\n-----END PRIVATE KEY-----", "MIIEvQIBADAN"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			redacted := Redact(testCase.input)
			if strings.Contains(redacted, testCase.leak) {
				t.Errorf("redaction leaked %q in %q", testCase.leak, redacted)
			}
			if !strings.Contains(redacted, "<redacted") {
				t.Errorf("redaction left no marker: %q", redacted)
			}
		})
	}
}

func TestJSONHandlerRedactsFields(t *testing.T) {
	restoreLogger(t)
	var buffer bytes.Buffer
	if err := Configure(Config{Format: "json", Level: LevelInfo, Output: &buffer}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	Structured(LevelInfo, "accepted", Fields("private_key", strings.Repeat("ab", 40), "tx_id", "deadbeef"))

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if record["private_key"] != "<redacted>" {
		t.Errorf("secret field not redacted: %v", record["private_key"])
	}
	if record["tx_id"] != "deadbeef" {
		t.Errorf("non-secret field changed: %v", record["tx_id"])
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("SAN_LOG_FORMAT", "json")
	t.Setenv("SAN_LOG_LEVEL", "debug")
	config, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if config.Format != "json" || config.Level != LevelDebug {
		t.Fatalf("unexpected config: %+v", config)
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("invalid level accepted")
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("invalid format accepted")
	}
}
