// Package sanlog configures the standard library logger for SAN Network
// nodes. It supports human-readable and JSON output, configurable levels and
// redaction of credentials.
//
// All existing code logs through the standard "log" package, so configuring
// the output handler here upgrades every log line at once. New code can use
// Structured to attach consistent fields (node id, chain id, peer, height,
// block hash, tx id, validator, round, error type).
package sanlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level is a log severity.
type Level int

// Levels, from most to least verbose.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String renders a level for output.
func (level Level) String() string {
	switch level {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel accepts debug/info/warn/warning/error (case-insensitive).
func ParseLevel(raw string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return LevelInfo, nil
	case "debug", "trace":
		return LevelDebug, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error", "fatal", "critical":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", raw)
	}
}

// ParseFormat accepts text|json (case-insensitive).
func ParseFormat(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "text", "plain", "console":
		return "text", nil
	case "json", "structured":
		return "json", nil
	default:
		return "text", fmt.Errorf("invalid log format %q (want text or json)", raw)
	}
}

// Config is the resolved logger configuration.
type Config struct {
	Format string
	Level  Level
	Output io.Writer
	// Timestamps controls the RFC 3339 field in JSON mode (disabled on the
	// text handler, which keeps the standard library timestamp).
	Timestamps bool
}

// DefaultConfig returns text output at info level on stderr.
func DefaultConfig() Config {
	return Config{Format: "text", Level: LevelInfo, Output: os.Stderr, Timestamps: true}
}

// FromEnv reads SAN_LOG_FORMAT and SAN_LOG_LEVEL.
func FromEnv() (Config, error) {
	config := DefaultConfig()
	if raw := strings.TrimSpace(os.Getenv("SAN_LOG_FORMAT")); raw != "" {
		format, err := ParseFormat(raw)
		if err != nil {
			return config, err
		}
		config.Format = format
	}
	if raw := strings.TrimSpace(os.Getenv("SAN_LOG_LEVEL")); raw != "" {
		level, err := ParseLevel(raw)
		if err != nil {
			return config, err
		}
		config.Level = level
	}
	if raw := strings.TrimSpace(os.Getenv("SAN_LOG_TIMESTAMPS")); raw != "" {
		config.Timestamps = raw == "1" || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "yes")
	}
	return config, nil
}

// handlerConfigured reports whether Configure installed the handler; used to
// choose between the structured marker and a readable fallback.
var handlerConfigured atomic.Bool

// Configure installs the handler as the standard library logger output.
// Calling Configure again replaces the previous handler.
func Configure(config Config) error {
	format, err := ParseFormat(config.Format)
	if err != nil {
		return err
	}
	config.Format = format
	if config.Output == nil {
		config.Output = os.Stderr
	}
	handler := &handler{
		format:     config.Format,
		level:      config.Level,
		out:        config.Output,
		timestamps: config.Timestamps,
	}
	// The standard library adds the date/time; the default flags keep the
	// human form close to the original output.
	log.SetFlags(log.LstdFlags)
	log.SetOutput(handler)
	handlerConfigured.Store(true)
	return nil
}

// ConfigureFromEnv is the default process entry point.
func ConfigureFromEnv() (Config, error) {
	config, err := FromEnv()
	if err != nil {
		return config, err
	}
	if err := Configure(config); err != nil {
		return config, err
	}
	return config, nil
}

// fieldsMarker separates the human message from the structured fields payload
// written by Structured. The record separator (0x1e) never appears in regular
// log messages.
const fieldsMarker = "\x1e"

// Fields builds a structured field map from alternating key/value pairs.
// Non-string keys or an odd number of values are ignored.
func Fields(keyValues ...any) map[string]any {
	fields := map[string]any{}
	for index := 0; index+1 < len(keyValues); index += 2 {
		key, ok := keyValues[index].(string)
		if !ok || key == "" {
			continue
		}
		fields[key] = keyValues[index+1]
	}
	return fields
}

// Structured emits one record with attached fields. It goes through the
// standard logger, so it honours the configured format and level. When no
// handler is installed it still prints a readable line.
func Structured(level Level, message string, fields map[string]any) {
	payload := message
	if len(fields) > 0 {
		if handlerConfigured.Load() {
			encoded, err := json.Marshal(fields)
			if err == nil {
				payload = message + fieldsMarker + string(encoded)
			}
		} else {
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			var readable strings.Builder
			readable.WriteString(message)
			for _, key := range keys {
				readable.WriteString(" ")
				readable.WriteString(key)
				readable.WriteString("=")
				readable.WriteString(fmt.Sprintf("%v", fields[key]))
			}
			payload = readable.String()
		}
	}
	record := "[" + strings.ToUpper(level.String()) + "] " + payload
	// log.Output(2, ...) would point callers at this package; depth 2 shows
	// the caller when the handler wants it, but the standard output is what
	// matters here.
	_ = log.Output(2, record)
}

// Debugf/Infof/Warnf/Errorf are printf-style structured helpers.
func Debugf(format string, args ...any) { Structured(LevelDebug, fmt.Sprintf(format, args...), nil) }
func Infof(format string, args ...any)  { Structured(LevelInfo, fmt.Sprintf(format, args...), nil) }
func Warnf(format string, args ...any)  { Structured(LevelWarn, fmt.Sprintf(format, args...), nil) }
func Errorf(format string, args ...any) { Structured(LevelError, fmt.Sprintf(format, args...), nil) }

// Debug/Info/Warn/Error attach fields to a message.
func Debug(message string, fields map[string]any) { Structured(LevelDebug, message, fields) }
func Info(message string, fields map[string]any)  { Structured(LevelInfo, message, fields) }
func Warn(message string, fields map[string]any)  { Structured(LevelWarn, message, fields) }
func Error(message string, fields map[string]any) { Structured(LevelError, message, fields) }

// ---------------------------------------------------------------------- #
// Redaction
// ---------------------------------------------------------------------- #

var redactionRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	// JSON-style secret assignments: "private_key": "....".
	{regexp.MustCompile(`(?i)("(?:private_key|secret_key|api_token|auth_token|token|password|passphrase|mnemonic|seed_phrase|seed|tls_key)"\s*:\s*")[^"]*(")`), `${1}<redacted>${2}`},
	// key=value / key: value secret assignments in human log lines.
	{regexp.MustCompile(`(?i)((?:api[_-]?token|auth[_-]?token|access[_-]?token|token|password|passphrase|private[_-]?key|secret[_-]?key|tls[_-]?key|secret)\s*[:=]\s*)([^\s,;"'}\]]+)`), `${1}<redacted>`},
	// Bearer tokens in headers or URLs.
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s"']+`), `${1}<redacted>`},
	{regexp.MustCompile(`(?i)([?&](?:token|api_token|access_token)=)[^&\s]+`), `${1}<redacted>`},
	// PEM private key blocks (multi-line).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), `<redacted private key>`},
	// Hex private keys labelled as such regardless of quoting style.
	{regexp.MustCompile(`(?i)((?:private|secret)[_-]?key\s*[:=]\s*)[0-9a-f]{64,}`), `${1}<redacted>`},
}

// Redact removes credentials from a log line. It is safe on arbitrary text
// and never panics.
func Redact(text string) string {
	for _, rule := range redactionRules {
		text = rule.pattern.ReplaceAllString(text, rule.replace)
	}
	return text
}

// ---------------------------------------------------------------------- #
// Handler
// ---------------------------------------------------------------------- #

type handler struct {
	mu         sync.Mutex
	format     string
	level      Level
	out        io.Writer
	timestamps bool
}

// Write implements io.Writer for the standard library log package. Each call
// is one record.
func (h *handler) Write(payload []byte) (int, error) {
	raw, fields := splitFields(strings.TrimRight(string(payload), "\r\n"))
	timestampPrefix := stdlibTimestamp.FindString(raw)
	normalized := stripStdlibTimestamp(raw)
	level := detectLevel(normalized)
	if level < h.level {
		return len(payload), nil
	}
	message := Redact(stripLevelPrefix(normalized))
	fields = redactFields(fields)

	var line []byte
	if h.format == "json" {
		line = h.renderJSON(level, message, fields)
	} else {
		line = h.renderText(timestampPrefix+message, fields)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.out.Write(line); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func (h *handler) renderText(message string, fields map[string]any) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(message)
	for key, value := range fields {
		buffer.WriteString(" ")
		buffer.WriteString(key)
		buffer.WriteString("=")
		buffer.WriteString(fmt.Sprintf("%v", value))
	}
	buffer.WriteString("\n")
	return buffer.Bytes()
}

// stdlibTimestamp matches "2006/01/02 15:04:05 " at the start of a record.
var stdlibTimestamp = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

func stripStdlibTimestamp(message string) string {
	return stdlibTimestamp.ReplaceAllString(message, "")
}

func (h *handler) renderJSON(level Level, message string, fields map[string]any) []byte {
	record := map[string]any{"level": level.String(), "msg": message}
	if h.timestamps {
		record["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	for key, value := range fields {
		record[key] = value
	}
	buffer := bytes.NewBuffer(nil)
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(record)
	return buffer.Bytes()
}

func splitFields(line string) (string, map[string]any) {
	index := strings.Index(line, fieldsMarker)
	if index < 0 {
		return line, nil
	}
	message := line[:index]
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(line[index+len(fieldsMarker):]), &fields); err != nil {
		return line, nil
	}
	return message, fields
}

func redactFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return fields
	}
	sanitized := make(map[string]any, len(fields))
	for key, value := range fields {
		if isSecretKey(key) {
			sanitized[key] = "<redacted>"
			continue
		}
		if text, ok := value.(string); ok {
			sanitized[key] = Redact(text)
			continue
		}
		sanitized[key] = value
	}
	return sanitized
}

func isSecretKey(key string) bool {
	normalized := strings.ToLower(key)
	for _, secret := range []string{"private", "secret", "token", "password", "passphrase", "mnemonic", "seed_phrase"} {
		if strings.Contains(normalized, secret) {
			return true
		}
	}
	return false
}

// detectLevel reads the explicit "[LEVEL]" prefix written by Structured and
// falls back to common message prefixes from legacy log.Printf calls.
func detectLevel(message string) Level {
	upper := strings.ToUpper(message)
	switch {
	case strings.HasPrefix(upper, "[DEBUG]"):
		return LevelDebug
	case strings.HasPrefix(upper, "[WARN]"), strings.HasPrefix(upper, "WARNING"):
		return LevelWarn
	case strings.HasPrefix(upper, "[ERROR]"), strings.HasPrefix(upper, "ERROR"), strings.HasPrefix(upper, "FATAL"):
		return LevelError
	default:
		return LevelInfo
	}
}

func stripLevelPrefix(message string) string {
	for _, prefix := range []string{"[DEBUG] ", "[INFO] ", "[WARN] ", "[ERROR] "} {
		if strings.HasPrefix(message, prefix) {
			return strings.TrimPrefix(message, prefix)
		}
	}
	return message
}
