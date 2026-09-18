// Package deploy contains lint/parse tests for the deployment assets: the
// systemd unit, the Dockerfile and the installer. They run on every platform
// because they only inspect the files (no systemd/docker required).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// parseUnit splits a systemd unit into section -> key -> value.
func parseUnit(t *testing.T, content string) map[string]map[string]string {
	t.Helper()
	sections := map[string]map[string]string{}
	current := ""
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.Trim(line, "[]")
			if sections[current] == nil {
				sections[current] = map[string]string{}
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("malformed unit line %q", line)
		}
		if current == "" {
			t.Fatalf("unit line outside a section: %q", line)
		}
		sections[current][strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return sections
}

func TestSystemdUnitHardening(t *testing.T) {
	unit := parseUnit(t, readAsset(t, "san-node.service"))
	service, ok := unit["Service"]
	if !ok {
		t.Fatal("missing [Service] section")
	}
	if service["ExecStart"] != "@PREFIX@/bin/sanup --foreground" {
		t.Errorf("ExecStart = %q", service["ExecStart"])
	}
	if service["KillSignal"] != "SIGTERM" || service["TimeoutStopSec"] == "" {
		t.Errorf("graceful shutdown not configured: KillSignal=%q TimeoutStopSec=%q",
			service["KillSignal"], service["TimeoutStopSec"])
	}
	if service["Restart"] != "on-failure" {
		t.Errorf("Restart = %q, want on-failure", service["Restart"])
	}
	required := []string{
		"NoNewPrivileges", "ProtectSystem", "ProtectHome", "PrivateTmp",
		"ProtectKernelTunables", "ProtectKernelModules", "ProtectControlGroups",
		"RestrictSUIDSGID", "RestrictNamespaces", "RestrictAddressFamilies",
		"CapabilityBoundingSet", "LimitNOFILE", "ReadWritePaths", "UMask",
	}
	for _, key := range required {
		if _, present := service[key]; !present {
			t.Errorf("hardening key %s missing", key)
		}
	}
	if service["ReadWritePaths"] != "@DATA_DIR@" {
		t.Errorf("ReadWritePaths = %q; LMDB needs the data dir writable", service["ReadWritePaths"])
	}
	if _, present := service["WatchdogSec"]; present {
		t.Error("WatchdogSec requires sd_notify support, which the node does not implement")
	}
	if !strings.Contains(service["EnvironmentFile"], "san.env") {
		t.Errorf("EnvironmentFile = %q", service["EnvironmentFile"])
	}
	if !strings.Contains(unit["Unit"]["Documentation"], "san_network") {
		t.Errorf("Documentation = %q", unit["Unit"]["Documentation"])
	}
	if _, present := unit["Install"]; !present {
		t.Error("missing [Install] section")
	}
}

func TestDockerfileRuntimeContract(t *testing.T) {
	dockerfile := readAsset(t, "Dockerfile")
	for _, expected := range []string{
		"gcr.io/distroless/base-debian12:nonroot",
		"STOPSIGNAL SIGTERM",
		"HEALTHCHECK",
		"VOLUME [\"/var/lib/san\"]",
		"EXPOSE 8000 8765 8769 8770",
		"SAN_LOG_FORMAT=json",
		"internal/version.Version=",
		"internal/version.Commit=",
		"internal/version.BuildDate=",
		"CGO_ENABLED=1",
		"-tags lmdb",
	} {
		if !strings.Contains(dockerfile, expected) {
			t.Errorf("Dockerfile missing %q", expected)
		}
	}
	if strings.Contains(dockerfile, "USER root") {
		t.Error("Dockerfile must not run as root")
	}
}

func TestInstallScriptInjectsVersionAndKeepsGracefulStop(t *testing.T) {
	script := readAsset(t, "install.sh")
	for _, expected := range []string{
		"--foreground",
		"internal/version.Version=",
		"internal/version.Commit=",
		"internal/version.BuildDate=",
		"systemctl restart san-node.service",
		"WITH_CERT",
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("install.sh missing %q", expected)
		}
	}
	if bash, err := exec.LookPath("bash"); err == nil && runtime.GOOS != "windows" {
		command := exec.Command(bash, "-n", filepath.Join(mustGetwd(t), "install.sh"))
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("install.sh syntax error: %v\n%s", err, output)
		}
	}
}

func TestEnvExampleDocumentsLoggingAndReadiness(t *testing.T) {
	env := readAsset(t, "san.env.example")
	for _, expected := range []string{
		"SAN_LOG_FORMAT=", "SAN_LOG_LEVEL=", "GET /ready", "SAN_API_TOKEN",
		"SAN_TLS_CERT=", "SAN_DB_BACKEND=lmdb",
	} {
		if !strings.Contains(env, expected) {
			t.Errorf("san.env.example missing %q", expected)
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return directory
}
