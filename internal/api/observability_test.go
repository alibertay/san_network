package api_test

import (
	"net/http"
	"testing"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/netnode"
)

// TestReadyEndpointReportsStarting covers the lifecycle state before the node
// is started: /ready is 503 with state "starting".
func TestReadyEndpointReportsStarting(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/ready", "", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", recorder.Code)
	}
	body := decodeObject(t, recorder)
	if body["state"] != "starting" || body["ready"] != false {
		t.Errorf("unexpected readiness payload: %v", body)
	}
	checks, _ := body["checks"].(map[string]any)
	if checks == nil {
		t.Fatalf("checks missing: %v", body)
	}
	for _, name := range []string{
		"database_loaded", "genesis_verified", "identity_loaded",
		"p2p_listeners_up", "synced", "peers_available",
		"controllers_available", "public_devnet",
	} {
		if _, present := checks[name]; !present {
			t.Errorf("readiness check %q missing", name)
		}
	}
	if checks["identity_loaded"] != true {
		t.Errorf("identity_loaded should be true, got %v", checks["identity_loaded"])
	}
}

// TestReadyEndpointMissingNode is the requireNode 503 path.
func TestReadyEndpointMissingNode(t *testing.T) {
	server := api.NewServer(nil, testConfig())
	recorder := doRequest(t, server, http.MethodGet, "/ready", "", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", recorder.Code)
	}
}

// TestHealthExposesVersionInfo checks the build metadata on /health.
func TestHealthExposesVersionInfo(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	body := decodeObject(t, doRequest(t, server, http.MethodGet, "/health", "", nil))
	info, _ := body["version_info"].(map[string]any)
	if info == nil {
		t.Fatalf("version_info missing: %v", body)
	}
	for _, key := range []string{"version", "commit", "build_date", "protocol_version", "schema_version", "go_version"} {
		if _, present := info[key]; !present {
			t.Errorf("version_info.%s missing", key)
		}
	}
	if info["protocol_version"].(float64) != float64(netnode.ProtocolVersion) {
		t.Errorf("protocol_version = %v", info["protocol_version"])
	}
}
