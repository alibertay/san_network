package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestGlobalParsing(t *testing.T) {
	options, rest, exit := parseGlobal([]string{
		"--rpc", "http://example:9000", "--key", "wallet.json", "--timeout", "2.5", "health",
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != -1 {
		t.Fatalf("exit: got %d", exit)
	}
	if options.rpc != "http://example:9000" || options.key != "wallet.json" || options.timeout != 2.5 {
		t.Fatalf("options: %+v", options)
	}
	if len(rest) != 1 || rest[0] != "health" {
		t.Fatalf("rest: %v", rest)
	}
}

func TestGlobalParsingEquals(t *testing.T) {
	options, rest, exit := parseGlobal([]string{"--rpc=http://x", "--timeout=1.5", "metrics"}, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != -1 || len(rest) != 1 || rest[0] != "metrics" {
		t.Fatalf("parse: exit=%d rest=%v", exit, rest)
	}
	if options.rpc != "http://x" || options.timeout != 1.5 {
		t.Fatalf("options: %+v", options)
	}
}

func TestGlobalParsingErrors(t *testing.T) {
	_, _, exit := parseGlobal([]string{"--nope", "health"}, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != 2 {
		t.Fatalf("unknown flag exit: got %d, want 2", exit)
	}
	_, _, exit = parseGlobal([]string{"--timeout", "abc", "health"}, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != 2 {
		t.Fatalf("bad timeout exit: got %d, want 2", exit)
	}
}

func TestHelpExitsZero(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"--help"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("help exit: got %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:8000") {
		t.Errorf("help text missing default rpc: %s", stdout.String())
	}
}

func TestSignCommandRequiresKey(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"send", "--to", "0xabc", "--value", "1"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit: got %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--key is required for transaction commands") {
		t.Errorf("stderr: %s", stderr.String())
	}
}

func TestParseSend(t *testing.T) {
	options, code := parseSend([]string{"--to", "0xabc", "--value", "2.5", "--nonce", "7"}, &bytes.Buffer{})
	if code != -1 {
		t.Fatalf("code: got %d", code)
	}
	if options.to != "0xabc" || options.value != 2.5 {
		t.Fatalf("options: %+v", options)
	}
	if options.nonce == nil || *options.nonce != 7 {
		t.Fatalf("nonce: %v", options.nonce)
	}

	_, code = parseSend([]string{"--to", "0xabc"}, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("missing value code: got %d, want 2", code)
	}
}

func TestParseCallAndQuery(t *testing.T) {
	call, code := parseCall([]string{
		"--id", "kv", "--function", "set", "--param", "alice", "--param", "1", "--gas", "5000",
	}, &bytes.Buffer{})
	if code != -1 {
		t.Fatalf("call code: got %d", code)
	}
	if call.id != "kv" || call.function != "set" || call.gas != 5000 || len(call.params) != 2 {
		t.Fatalf("call options: %+v", call)
	}
	coerced := coerceParams(call.params)
	if coerced[0] != "alice" || coerced[1] != int64(1) {
		t.Fatalf("coerced params: %v", coerced)
	}

	query, code := parseQuery([]string{"--id", "kv", "--function", "get", "--param", "alice"}, &bytes.Buffer{})
	if code != -1 || query.id != "kv" || query.function != "get" || len(query.params) != 1 {
		t.Fatalf("query options: %+v (code %d)", query, code)
	}
}

func TestParseReceiptRequiresBoth(t *testing.T) {
	options, code := parseReceipt([]string{"--block", "1", "--tx", "2"}, &bytes.Buffer{})
	if code != -1 || options.block != 1 || options.tx != 2 {
		t.Fatalf("receipt options: %+v (code %d)", options, code)
	}
	_, code = parseReceipt([]string{"--block", "1"}, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("missing tx code: got %d, want 2", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"frobnicate"}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit: got %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("stderr: %s", stderr.String())
	}
}
