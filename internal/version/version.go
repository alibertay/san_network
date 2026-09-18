// Package version carries the build metadata exposed by the CLI and the REST
// API: semantic version, git commit, build date, protocol version, database
// schema version and the Go toolchain.
//
// Version, Commit and BuildDate are overridable at link time:
//
//	go build -ldflags "-X github.com/alibertay/san_network/internal/version.Version=1.2.3 \
//	                    -X github.com/alibertay/san_network/internal/version.Commit=$(git rev-parse HEAD) \
//	                    -X github.com/alibertay/san_network/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// When they are not injected the package falls back to the module build info,
// so `go run ./cmd/sannode version` still reports something useful.
package version

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Linker-overridable build metadata.
var (
	// Version is the semantic version of the release ("dev" when built from
	// a working tree without an injected value).
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = ""
	// BuildDate is the UTC build timestamp (RFC 3339).
	BuildDate = ""
)

// ProtocolName is the P2P protocol family.
const ProtocolName = "san-p2p"

// Metadata is the resolved build information.
type Metadata struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildDate       string `json:"build_date"`
	ProtocolName    string `json:"protocol_name"`
	ProtocolVersion int    `json:"protocol_version"`
	SchemaVersion   int    `json:"schema_version"`
	GoVersion       string `json:"go_version"`
}

// Resolve fills a Metadata with the process build info. protocolVersion and
// schemaVersion are passed by the caller so this package stays dependency
// free (and cannot introduce an import cycle with netnode/ledger).
func Resolve(protocolVersion, schemaVersion int) Metadata {
	if build, ok := debug.ReadBuildInfo(); ok {
		if Commit == "" {
			for _, setting := range build.Settings {
				if setting.Key == "vcs.revision" {
					Commit = setting.Value
					break
				}
			}
		}
		if Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			Version = strings.TrimPrefix(build.Main.Version, "v")
		}
		if BuildDate == "" {
			for _, setting := range build.Settings {
				if setting.Key == "vcs.time" {
					BuildDate = setting.Value
					break
				}
			}
		}
	}
	return Metadata{
		Version:         Version,
		Commit:          Commit,
		BuildDate:       BuildDate,
		ProtocolName:    ProtocolName,
		ProtocolVersion: protocolVersion,
		SchemaVersion:   schemaVersion,
		GoVersion:       runtime.Version(),
	}
}

// ShortCommit truncates the commit to 12 characters for compact output.
func ShortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// Map renders the metadata as a JSON-friendly object (canonical marshal
// compatible: only strings and integers).
func (metadata Metadata) Map() map[string]any {
	return map[string]any{
		"version":          metadata.Version,
		"commit":           metadata.Commit,
		"build_date":       metadata.BuildDate,
		"protocol_name":    metadata.ProtocolName,
		"protocol_version": int64(metadata.ProtocolVersion),
		"schema_version":   int64(metadata.SchemaVersion),
		"go_version":       metadata.GoVersion,
	}
}

// String renders the one-line human form used by `<binary> version`.
func (metadata Metadata) String() string {
	commit := metadata.Commit
	if commit == "" {
		commit = "unknown"
	} else {
		commit = ShortCommit(commit)
	}
	buildDate := metadata.BuildDate
	if buildDate == "" {
		buildDate = "unknown"
	}
	return "san " + metadata.Version +
		" (commit " + commit +
		", built " + buildDate +
		", protocol " + metadata.ProtocolName + "/" + itoa(metadata.ProtocolVersion) +
		", schema " + itoa(metadata.SchemaVersion) +
		", " + metadata.GoVersion + ")"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
