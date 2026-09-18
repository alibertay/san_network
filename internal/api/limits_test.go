package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
)

// TestBodyLimitIncrementsRejectionCounter covers the chunked-body cap and the
// Go-only http_body_rejected counter.
func TestBodyLimitIncrementsRejectionCounter(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCMaxBody = 64
	server := api.NewServer(node, config)

	before := api.BodyLimitRejections()
	body := `{"pad":"` + strings.Repeat("x", 4096) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/transaction", strings.NewReader(body))
	request.ContentLength = -1
	request.Header.Del("Content-Length")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status %d, want 413", recorder.Code)
	}
	if got := api.BodyLimitRejections(); got <= before {
		t.Fatalf("http body rejection counter did not advance: %d -> %d", before, got)
	}
}

// TestContentLengthGateIncrementsRejectionCounter covers the header gate.
func TestContentLengthGateIncrementsRejectionCounter(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCMaxBody = 64
	server := api.NewServer(node, config)

	before := api.BodyLimitRejections()
	request := httptest.NewRequest(http.MethodPost, "/transaction", strings.NewReader(`{}`))
	request.Header.Set("Content-Length", "4096")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("lying content-length status %d, want 413", recorder.Code)
	}
	if got := api.BodyLimitRejections(); got <= before {
		t.Fatalf("content-length rejection counter did not advance: %d -> %d", before, got)
	}
}
