package sanlog

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactionCoversEnvironmentAndTLSSecrets extends the credential matrix
// with the environment-style values a public node actually carries (SAN_*
// tokens, TLS PEM blocks) and pins that the process environment is never
// dumped by the structured logger.
func TestRedactionCoversEnvironmentAndTLSSecrets(t *testing.T) {
	ecKey := "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBx0\n-----END EC PRIVATE KEY-----"
	apiToken := "SAN_API_TOKEN=abcdef1234567890 SAN_PUBLIC_DEVNET=1"
	tlsKey := "SAN_TLS_KEY=/etc/san/certs/node.key"
	cases := []struct {
		name  string
		input string
		leak  string
	}{
		{"environment api token", apiToken, "abcdef1234567890"},
		{"environment tls key path assignment", tlsKey, "/etc/san/certs/node.key"},
		{"ec private key pem", ecKey, "MHcCAQEEIBx0"},
		{"exported token", "export SAN_API_TOKEN=topsecretvalue", "topsecretvalue"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			redacted := Redact(testCase.input)
			if strings.Contains(redacted, testCase.leak) {
				t.Errorf("redaction leaked %q in %q", testCase.leak, redacted)
			}
		})
	}
}

// TestStructuredLoggerRedactsEnvironmentField pins that even if a caller logs
// a whole environment-style string as a field value, credentials do not reach
// the output.
func TestStructuredLoggerRedactsEnvironmentField(t *testing.T) {
	restoreLogger(t)
	var buffer bytes.Buffer
	if err := Configure(Config{Format: "json", Level: LevelInfo, Output: &buffer}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	Structured(LevelInfo, "diagnostics", Fields(
		"environment", "HOME=/root SAN_API_TOKEN=supersecrettoken SAN_LOG_LEVEL=info",
		"san_api_token", "supersecrettoken",
	))
	output := buffer.String()
	if strings.Contains(output, "supersecrettoken") {
		t.Fatalf("structured log leaked a secret: %s", output)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buffer.Bytes()), &record); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if record["san_api_token"] != "<redacted>" {
		t.Fatalf("secret field not redacted: %v", record["san_api_token"])
	}
}
