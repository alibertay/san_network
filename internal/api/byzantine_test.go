package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

// TestByzantineRESTEndpointsRejectHostileInput drives the public REST surface
// with malformed and adversarial requests and checks every one is answered
// cleanly with the node state unchanged.
func TestByzantineRESTEndpointsRejectHostileInput(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	hostileBodies := []string{
		"",
		"{",
		"null",
		"[]",
		"{\"chain_id\":1}",
		"{\"sender\":[],\"nonce\":{}}",
		"{\"contract_id\":1,\"function_name\":null}",
		strings.Repeat("A", 4096),
		strings.Repeat("{\"a\":", 2000) + "1" + strings.Repeat("}", 2000),
	}
	for _, body := range hostileBodies {
		for _, path := range []string{"/transaction", "/contract/query", "/faucet"} {
			recorder := doRequest(t, server, http.MethodPost, path, body, nil)
			if recorder.Code == 0 {
				t.Fatalf("no response for %s %q", path, body)
			}
		}
	}

	requests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/account/not-an-address", http.StatusBadRequest},
		{http.MethodGet, "/account/0x" + strings.Repeat("zz", 20), http.StatusBadRequest},
		{http.MethodGet, "/block/-1", http.StatusNotFound},
		{http.MethodGet, "/block/not-a-number", http.StatusUnprocessableEntity},
		{http.MethodGet, "/proof/tx/1/not-a-number", http.StatusUnprocessableEntity},
		{http.MethodGet, "/account/", http.StatusNotFound},
		{http.MethodGet, "/does-not-exist", http.StatusNotFound},
		{http.MethodDelete, "/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/" + strings.Repeat("x", 2048), http.StatusNotFound},
		{http.MethodGet, "/tx/" + strings.Repeat("ab", 64), http.StatusNotFound},
	}
	for _, request := range requests {
		recorder := doRequest(t, server, request.method, request.path, "", nil)
		if recorder.Code != request.want {
			t.Errorf("%s %s: got %d, want %d (%s)", request.method, request.path, recorder.Code, request.want, recorder.Body.String())
		}
	}

	if got := node.Tip().Index; got != 0 {
		t.Fatalf("hostile REST traffic changed the chain height: %d", got)
	}
	if node.PendingCount() != 0 {
		t.Fatalf("hostile REST traffic entered the mempool: %d", node.PendingCount())
	}
}

// TestByzantineOversizedRequestWithoutContentLength ensures a streamed body
// larger than RPCMaxBody is cut off without buffering unbounded memory.
func TestByzantineOversizedRequestWithoutContentLength(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCMaxBody = 1024
	server := api.NewServer(node, config)

	request := httptest.NewRequest(http.MethodPost, "/transaction", strings.NewReader(strings.Repeat("x", 8*1024)))
	request.ContentLength = -1
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge && recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 413/400", recorder.Code)
	}
}

// TestByzantineMalformedQueryParameters checks integer parsing never panics
// and always maps to a 400 rather than a wrong result.
func TestByzantineMalformedQueryParameters(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)
	for _, query := range []string{
		"from_index=-1",
		"from_index=abc",
		"limit=0",
		"limit=99999999999999999999",
		"limit=-5",
	} {
		recorder := doRequest(t, server, http.MethodGet, "/headers?"+query, "", nil)
		if recorder.Code == http.StatusOK {
			t.Errorf("malformed query %q was accepted", query)
		}
	}
	for _, limit := range []string{"0", "-1", "99999999999999999999"} {
		recorder := doRequest(t, server, http.MethodGet, "/sync?limit="+limit, "", nil)
		if recorder.Code == http.StatusOK {
			t.Errorf("malformed sync limit %q was accepted", limit)
		}
	}
}

// TestByzantineMetricsNeverExposeSecrets binds the exposition format to plain
// numbers even when the snapshot contains hostile value types.
func TestByzantineMetricsNeverExposeSecrets(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)
	recorder := doRequest(t, server, http.MethodGet, "/metrics", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "private") || strings.Contains(recorder.Body.String(), "token") {
		t.Fatalf("metrics leaked sensitive keys: %s", recorder.Body.String())
	}
}

func TestByzantineDeepNestingDoesNotPanic(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)
	body := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	recorder := doRequest(t, server, http.MethodPost, "/transaction", body, nil)
	if recorder.Code == http.StatusOK {
		t.Fatalf("deeply nested body was accepted")
	}
}

func TestByzantineErrorMessagesDoNotEchoRawInput(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)
	secret := "super-secret-key-material"
	body := fmt.Sprintf(`{"chain_id":"san-devnet-1","sender":"%s"}`, secret)
	recorder := doRequest(t, server, http.MethodPost, "/transaction", body, nil)
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("error response echoed attacker-controlled input: %s", recorder.Body.String())
	}
}
