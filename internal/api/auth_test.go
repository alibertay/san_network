package api_test

import (
	"net/http"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

// TestAPITokenEnforcedWhenConfigured covers SAN_API_TOKEN: no token and a
// wrong token are rejected with the FastAPI error shape, the correct bearer
// token is accepted.
func TestAPITokenEnforcedWhenConfigured(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.APIToken = "s3cret-token"
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/health", "", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d, want 401 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeObject(t, recorder)
	if body["detail"] != "Unauthorized" {
		t.Fatalf("missing token body: %v", body)
	}

	recorder = doRequest(t, server, http.MethodGet, "/health", "",
		map[string]string{"Authorization": "Bearer wrong-token"})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", recorder.Code)
	}

	recorder = doRequest(t, server, http.MethodGet, "/health", "",
		map[string]string{"Authorization": "Bearer s3cret-token"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("correct token: got %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	// A mutating route is protected as well.
	recorder = doRequest(t, server, http.MethodPost, "/transaction", `{}`, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("POST without token: got %d, want 401", recorder.Code)
	}
}

// TestAPIStaysOpenWithoutToken guards the devnet default.
func TestAPIStaysOpenWithoutToken(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.APIToken = ""
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/health", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unauthenticated /health: got %d, want 200", recorder.Code)
	}
}
