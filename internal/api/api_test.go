package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

const testChainID = "san-devnet-1"

func testConfig() netnode.NodeConfig {
	config := netnode.DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.ChainID = testChainID
	config.DBBackend = "memory"
	config.BlockThresholdFee = 0
	config.PeerCheckInterval = 3600
	return config
}

func newTestNode(t *testing.T, allocations map[string]int64) (*netnode.Node, *ledger.NodeIdentity, netnode.NodeConfig) {
	t.Helper()
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	config := testConfig()
	config.GenesisAllocations = allocations
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node, identity, config
}

func doRequest(t *testing.T, server *api.Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func decodeObject(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("cannot decode response %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func signPayload(t *testing.T, identity *ledger.NodeIdentity, payload map[string]any) map[string]any {
	t.Helper()
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		t.Fatalf("SignPayload: %v", err)
	}
	payload["signature"] = signature
	return payload
}

// TestHealth covers GET /health.
func TestHealth(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/health", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeObject(t, recorder)
	if body["status"] != "ok" {
		t.Errorf("status: got %v", body["status"])
	}
	if body["chain_id"] != testChainID {
		t.Errorf("chain_id: got %v", body["chain_id"])
	}
	if body["schema_version"].(float64) != float64(ledger.SchemaVersion) {
		t.Errorf("schema_version: got %v", body["schema_version"])
	}
	if body["height"].(float64) != 0 {
		t.Errorf("height: got %v", body["height"])
	}
	if body["peers"].(float64) != 0 || body["validators"].(float64) != 0 || body["mempool"].(float64) != 0 {
		t.Errorf("unexpected gauges: %v", body)
	}
	if body["tip_hash"] == "" || body["state_root"] == nil {
		t.Errorf("missing tip data: %v", body)
	}
}

// TestAccountLookup covers GET /account/{address} and its 400 path.
func TestAccountLookup(t *testing.T) {
	address := "0x" + strings.Repeat("ab", 20)
	node, _, config := newTestNode(t, map[string]int64{address: 3 * ledger.SANBase})
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/account/"+address, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeObject(t, recorder)
	if body["address"] != address {
		t.Errorf("address: got %v", body["address"])
	}
	if body["balance_units"].(float64) != float64(3*ledger.SANBase) {
		t.Errorf("balance_units: got %v", body["balance_units"])
	}
	if body["balance"] != "3" {
		t.Errorf("balance: got %v", body["balance"])
	}
	if body["nonce"].(float64) != 0 {
		t.Errorf("nonce: got %v", body["nonce"])
	}

	bad := doRequest(t, server, http.MethodGet, "/account/0xalice", "", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid address status: got %d, want 400", bad.Code)
	}
	if detail := decodeObject(t, bad)["detail"]; detail != "Invalid address: 0xalice" {
		t.Errorf("detail: got %v", detail)
	}
}

// TestSubmitTransaction covers POST /transaction (accepted, rejected, bad JSON).
func TestSubmitTransaction(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sender, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	receiver := "0x" + strings.Repeat("cd", 20)
	node, _, config := newTestNode(t, map[string]int64{sender: 100 * ledger.SANBase})
	server := api.NewServer(node, config)

	payload := signPayload(t, identity, map[string]any{
		"chain_id": testChainID,
		"sender":   identity.PublicKeyHex(),
		"nonce":    int64(0),
		"receiver": receiver,
		"value":    "1",
	})
	raw, _ := json.Marshal(payload)
	recorder := doRequest(t, server, http.MethodPost, "/transaction", string(raw), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d (%s)", recorder.Code, recorder.Body.String())
	}
	body := decodeObject(t, recorder)
	if body["status"] != "committed" {
		t.Fatalf("status: got %v (%v)", body["status"], body)
	}
	if body["tx_id"] == "" {
		t.Errorf("missing tx_id: %v", body)
	}
	if node.PendingCount() != 0 {
		t.Errorf("mempool should be empty after commit")
	}

	// A transaction with a broken signature is a 400 with the node's detail.
	tampered := signPayload(t, identity, map[string]any{
		"chain_id": testChainID,
		"sender":   identity.PublicKeyHex(),
		"nonce":    int64(1),
		"receiver": receiver,
		"value":    "1",
	})
	tampered["value"] = "2"
	rawTampered, _ := json.Marshal(tampered)
	rejected := doRequest(t, server, http.MethodPost, "/transaction", string(rawTampered), nil)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("tampered status: got %d, want 400", rejected.Code)
	}
	if detail := decodeObject(t, rejected)["detail"]; detail != "invalid transaction signature" {
		t.Errorf("tampered detail: got %v", detail)
	}

	// Malformed JSON is FastAPI's 422 validation error.
	malformed := doRequest(t, server, http.MethodPost, "/transaction", "not-json", nil)
	if malformed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed status: got %d, want 422", malformed.Code)
	}
	details, ok := decodeObject(t, malformed)["detail"].([]any)
	if !ok || len(details) == 0 {
		t.Fatalf("malformed detail: got %v", decodeObject(t, malformed))
	}
}

const tokenSource = `function answer() {
  return 42
}
function add(a, b) {
  return a + b
}
`

// TestContractDeployAndQuery covers deploy via /transaction, /contracts and
// /contract/query.
func TestContractDeployAndQuery(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sender, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	node, _, config := newTestNode(t, map[string]int64{sender: 100 * ledger.SANBase})
	server := api.NewServer(node, config)

	deploy := signPayload(t, identity, map[string]any{
		"chain_id":  testChainID,
		"sender":    identity.PublicKeyHex(),
		"nonce":     int64(0),
		"gas_limit": int64(2_000_000),
		"gas_price": int64(1),
		"contract_code": map[string]any{
			"command":     "deploy",
			"contract_id": "token",
			"pena_code":   tokenSource,
		},
	})
	rawDeploy, _ := json.Marshal(deploy)
	recorder := doRequest(t, server, http.MethodPost, "/transaction", string(rawDeploy), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deploy status: got %d (%s)", recorder.Code, recorder.Body.String())
	}
	if status := decodeObject(t, recorder)["status"]; status != "committed" {
		t.Fatalf("deploy result: %v", status)
	}

	contracts := doRequest(t, server, http.MethodGet, "/contracts", "", nil)
	if got := decodeObject(t, contracts)["contracts"].([]any); len(got) != 1 || got[0] != "token" {
		t.Fatalf("contracts: got %v", got)
	}

	answerBody := `{"contract_id":"token","function_name":"answer","params":[]}`
	answerRecorder := doRequest(t, server, http.MethodPost, "/contract/query", answerBody, nil)
	if answerRecorder.Code != http.StatusOK {
		t.Fatalf("answer status: got %d (%s)", answerRecorder.Code, answerRecorder.Body.String())
	}
	if result := decodeObject(t, answerRecorder)["result"].(float64); result != 42 {
		t.Errorf("answer result: got %v", result)
	}

	addBody := `{"contract_id":"token","function_name":"add","params":[2,3]}`
	addRecorder := doRequest(t, server, http.MethodPost, "/contract/query", addBody, nil)
	if addRecorder.Code != http.StatusOK {
		t.Fatalf("add status: got %d (%s)", addRecorder.Code, addRecorder.Body.String())
	}
	if result := decodeObject(t, addRecorder)["result"].(float64); result != 5 {
		t.Errorf("add result: got %v", result)
	}

	unknownBody := `{"contract_id":"nope","function_name":"get","params":[]}`
	unknown := doRequest(t, server, http.MethodPost, "/contract/query", unknownBody, nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown contract status: got %d, want 404", unknown.Code)
	}
	if detail := decodeObject(t, unknown)["detail"]; detail != "Unknown contract: nope" {
		t.Errorf("unknown contract detail: got %v", detail)
	}

	// Missing required field: 422 from the ContractQuery model.
	missing := doRequest(t, server, http.MethodPost, "/contract/query", `{"contract_id":"token"}`, nil)
	if missing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing field status: got %d, want 422", missing.Code)
	}
}

// TestRateLimit covers the 429 sliding window from app/limits.py.
func TestRateLimit(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCRateLimit = 3
	config.RPCRateWindow = 60
	config.RPCMaxBody = 1 << 20
	server := api.NewServer(node, config)

	codes := make([]int, 0, 6)
	for i := 0; i < 6; i++ {
		codes = append(codes, doRequest(t, server, http.MethodGet, "/health", "", nil).Code)
	}
	limited := 0
	for _, code := range codes {
		if code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited < 1 {
		t.Fatalf("expected at least one 429, got %v", codes)
	}
	rejected := doRequest(t, server, http.MethodGet, "/health", "", nil)
	if detail := decodeObject(t, rejected)["detail"]; detail != "rate limit exceeded" {
		t.Errorf("rate limit detail: got %v", detail)
	}
}

// TestBodyLimit covers the 413 / 400 Content-Length gate.
func TestBodyLimit(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.RPCRateLimit = 1000
	config.RPCMaxBody = 2000
	server := api.NewServer(node, config)

	oversized := doRequest(t, server, http.MethodPost, "/transaction",
		strings.Repeat("x", 5000), map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": "5000",
		})
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status: got %d, want 413", oversized.Code)
	}
	if detail := decodeObject(t, oversized)["detail"]; detail != "request body too large" {
		t.Errorf("oversized detail: got %v", detail)
	}

	invalid := doRequest(t, server, http.MethodGet, "/health", "",
		map[string]string{"Content-Length": "abc"})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid content-length status: got %d, want 400", invalid.Code)
	}
	if detail := decodeObject(t, invalid)["detail"]; detail != "invalid content-length" {
		t.Errorf("invalid content-length detail: got %v", detail)
	}
}

// TestQueryValidation covers FastAPI-style 422 errors for query/path params.
func TestQueryValidation(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	cases := []struct {
		path string
	}{
		{"/headers?from_index=-1"},
		{"/headers?limit=0"},
		{"/headers?from_index=abc"},
		{"/sync?limit=99999"},
		{"/proof/tx/not-an-int/0"},
		{"/block/1.5"},
	}
	for _, testCase := range cases {
		recorder := doRequest(t, server, http.MethodGet, testCase.path, "", nil)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d, want 422 (%s)", testCase.path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestMempoolAndUnknownLookups covers /mempool plus the 404 paths.
func TestMempoolAndUnknownLookups(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	mempool := doRequest(t, server, http.MethodGet, "/mempool", "", nil)
	if mempool.Code != http.StatusOK {
		t.Fatalf("mempool status: got %d", mempool.Code)
	}
	body := decodeObject(t, mempool)
	if body["count"].(float64) != 0 {
		t.Errorf("mempool count: got %v", body["count"])
	}
	txIDs, ok := body["tx_ids"].([]any)
	if !ok || len(txIDs) != 0 {
		t.Errorf("tx_ids: got %v", body["tx_ids"])
	}

	for path, want := range map[string]string{
		"/block/42":            "Block 42 not found",
		"/tx/deadbeef":         "Unknown transaction deadbeef",
		"/receipt/tx/deadbeef": "No receipt for deadbeef",
		"/receipt/9/9":         "No receipt available",
		"/proof/tx/9/9":        "No such transaction",
		"/snapshot":            "No finalized snapshot yet",
		"/proof/account/0x" + strings.Repeat("ee", 20): "No account proof for 0x" + strings.Repeat("ee", 20),
	} {
		recorder := doRequest(t, server, http.MethodGet, path, "", nil)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, recorder.Code)
			continue
		}
		if detail := decodeObject(t, recorder)["detail"]; detail != want {
			t.Errorf("%s: detail got %v, want %v", path, detail, want)
		}
	}
}

// TestMetricsEndpoint covers GET /metrics against RenderMetrics.
func TestMetricsEndpoint(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodGet, "/metrics", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("content type: got %q", contentType)
	}
	snapshot := node.MetricsSnapshot()
	snapshot["http_body_rejected"] = api.BodyLimitRejections()
	if got, want := recorder.Body.String(), api.RenderMetrics(snapshot); got != want {
		t.Errorf("metrics text mismatch:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(recorder.Body.String(), "san_height 0") {
		t.Errorf("missing san_height gauge")
	}
}

// TestServiceUnavailable covers the get_node 503 when no node is attached.
func TestServiceUnavailable(t *testing.T) {
	server := api.NewServer(nil, testConfig())
	recorder := doRequest(t, server, http.MethodGet, "/health", "", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", recorder.Code)
	}
	if detail := decodeObject(t, recorder)["detail"]; detail != "Node is not started" {
		t.Errorf("detail: got %v", detail)
	}
}

func TestGenesisAndFinalityShapes(t *testing.T) {
	address := "0x" + strings.Repeat("ab", 20)
	node, _, config := newTestNode(t, map[string]int64{address: 5 * ledger.SANBase})
	server := api.NewServer(node, config)

	genesis := doRequest(t, server, http.MethodGet, "/genesis", "", nil)
	if genesis.Code != http.StatusOK {
		t.Fatalf("genesis status: got %d", genesis.Code)
	}
	body := decodeObject(t, genesis)
	if body["chain_id"] != testChainID || body["genesis_hash"] == "" {
		t.Errorf("genesis: %v", body)
	}
	allocation := body["genesis_allocation"].(map[string]any)
	if allocation[address] != strconv.FormatInt(5*ledger.SANBase, 10) {
		t.Errorf("genesis allocation: %v", allocation)
	}
	if body["parameters"] == nil {
		t.Errorf("genesis parameters missing")
	}

	finality := doRequest(t, server, http.MethodGet, "/finality", "", nil)
	if finality.Code != http.StatusOK {
		t.Fatalf("finality status: got %d", finality.Code)
	}
	finalityBody := decodeObject(t, finality)
	if finalityBody["chain_id"] != testChainID {
		t.Errorf("finality chain_id: %v", finalityBody["chain_id"])
	}
}
