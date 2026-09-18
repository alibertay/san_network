package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

func guardNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v", name, recovered)
		}
	}()
	fn()
}

func serveRaw(server *api.Server, method, path, body, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if remoteAddr != "" {
		request.RemoteAddr = remoteAddr
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

// TestOversizedBodyRejectedWithoutContentLength covers the chunked-body path:
// the Content-Length gate cannot see the size, so the read side must cap it.
func TestOversizedBodyRejectedWithoutContentLength(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCMaxBody = 1024
	server := api.NewServer(node, config)

	body := `{"pad":"` + strings.Repeat("x", 64*1024) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/transaction", strings.NewReader(body))
	request.ContentLength = -1
	request.Header.Del("Content-Length")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized body: got %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}
}

// TestLyingContentLengthRejected covers a client that announces a small body
// and sends a large one.
func TestLyingContentLengthRejected(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCMaxBody = 1024
	server := api.NewServer(node, config)

	body := `{"pad":"` + strings.Repeat("x", 64*1024) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/transaction", strings.NewReader(body))
	request.Header.Set("Content-Length", "16")
	request.ContentLength = 16
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("lying content-length: got %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}
}

func TestMalformedBodiesNeverPanic(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	bodies := []string{
		"",
		"{",
		"[",
		"[]",
		"42",
		"null",
		`"string"`,
		`{"chain_id":[]}`,
		`{"chain_id":"san-devnet-1","sender":{},"nonce":[]}`,
		"\x00\xff\xfe",
		strings.Repeat("[", 500) + strings.Repeat("]", 500),
	}
	for index, body := range bodies {
		body := body
		guardNoPanic(t, "POST /transaction", func() {
			recorder := serveRaw(server, http.MethodPost, "/transaction", body, "", nil)
			if recorder.Code >= 500 {
				t.Errorf("case %d: server error %d for %q", index, recorder.Code, body)
			}
		})
		guardNoPanic(t, "POST /contract/query", func() {
			recorder := serveRaw(server, http.MethodPost, "/contract/query", body, "", nil)
			if recorder.Code >= 500 {
				t.Errorf("case %d: server error %d for %q", index, recorder.Code, body)
			}
		})
	}
}

// TestRateLimiterIgnoresForwardedFor proves a spoofed X-Forwarded-For header
// cannot reset the client bucket.
func TestRateLimiterIgnoresForwardedFor(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCRateLimit = 2
	config.RPCRateWindow = 30
	server := api.NewServer(node, config)

	for attempt := 0; attempt < 2; attempt++ {
		recorder := serveRaw(server, http.MethodGet, "/health", "", "10.0.0.9:5000",
			map[string]string{"X-Forwarded-For": fmt.Sprintf("1.2.3.%d", attempt)})
		if recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d: got %d, want 200", attempt, recorder.Code)
		}
	}
	recorder := serveRaw(server, http.MethodGet, "/health", "", "10.0.0.9:5000",
		map[string]string{"X-Forwarded-For": "5.6.7.8"})
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For bypassed the limit: got %d, want 429", recorder.Code)
	}

	other := serveRaw(server, http.MethodGet, "/health", "", "10.0.0.10:5000", nil)
	if other.Code != http.StatusOK {
		t.Fatalf("a different client IP must have its own bucket: got %d, want 200", other.Code)
	}
}
