package sdk_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/sdk"
)

func testConfig() netnode.NodeConfig {
	config := netnode.DefaultNodeConfig()
	config.Host = "127.0.0.1"
	config.ChainID = "san-devnet-1"
	config.DBBackend = "memory"
	config.PeerCheckInterval = 3600
	return config
}

// TestTransferVerifiesAndEntersMempool builds a transaction with the SDK,
// verifies its signature with ledger.VerifyTransaction and submits it through
// the real HTTP API handler into the node's mempool.
func TestTransferVerifiesAndEntersMempool(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	sender, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	config := testConfig()
	config.GenesisAllocations = map[string]int64{sender: 100 * ledger.SANBase}
	config.BlockThresholdFee = 500 // high enough to keep the transaction pooled
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	apiServer := api.NewServer(node, config)

	var mu sync.Mutex
	var captured map[string]any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			decoded, decodeErr := canonical.Decode(raw)
			if decodeErr == nil {
				if object, ok := decoded.(map[string]any); ok {
					mu.Lock()
					captured = object
					mu.Unlock()
				}
			}
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		apiServer.ServeHTTP(w, r)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := sdk.NewSanClient(server.URL, identity, time.Second)
	receiver := "0x" + strings.Repeat("42", 20)
	result, err := client.Transfer(receiver, float64(1), nil)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if result["status"] != "pooled" {
		t.Fatalf("status: got %v (%v)", result["status"], result)
	}

	mu.Lock()
	payload := captured
	mu.Unlock()
	if payload == nil {
		t.Fatalf("the SDK did not post a transaction payload")
	}
	if payload["chain_id"] != config.ChainID {
		t.Errorf("chain_id: got %v", payload["chain_id"])
	}
	if payload["sender"] != identity.PublicKeyHex() {
		t.Errorf("sender: got %v", payload["sender"])
	}
	if payload["nonce"].(int64) != 0 {
		t.Errorf("nonce: got %v", payload["nonce"])
	}
	if payload["receiver"] != receiver {
		t.Errorf("receiver: got %v", payload["receiver"])
	}
	if payload["signature"] == nil {
		t.Fatalf("unsigned payload: %v", payload)
	}
	if !ledger.VerifyTransaction(payload) {
		t.Fatalf("ledger.VerifyTransaction rejected the SDK payload: %v", payload)
	}
	if node.PendingCount() != 1 {
		t.Errorf("mempool size: got %d, want 1", node.PendingCount())
	}
}

// TestSendWithoutIdentity mirrors SanClientError for signing without a key.
func TestSendWithoutIdentity(t *testing.T) {
	client := sdk.NewSanClient("http://127.0.0.1:1", nil, time.Second)
	if _, err := client.Transfer("0x"+strings.Repeat("11", 20), 1, nil); err == nil {
		t.Fatalf("expected an error for a client without identity")
	}
}

// TestGovernanceApprovalSignature checks the approval signs the canonical
// message the node verifies in applyGovernanceCommand.
func TestGovernanceApprovalSignature(t *testing.T) {
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"chain_id":"san-devnet-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := sdk.NewSanClient(server.URL, identity, time.Second)
	nonce := int64(0)
	approval, err := client.GovernanceApproval("unbonding_period", 7, &nonce)
	if err != nil {
		t.Fatalf("GovernanceApproval: %v", err)
	}
	message, err := canonical.Marshal(map[string]any{
		"chain_id":  "san-devnet-1",
		"command":   "set_param",
		"name":      "unbonding_period",
		"value":     int64(7),
		"tx_sender": identity.PublicKeyHex(),
		"tx_nonce":  int64(0),
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !ledger.VerifyIdentity(message, approval["signature"].(string), approval["public_key"].(string)) {
		t.Fatalf("governance approval signature did not verify")
	}
}

// TestAccountProofVerification covers the static proof helper.
func TestAccountProofVerification(t *testing.T) {
	address := "0x" + strings.Repeat("77", 20)
	node, identity, config := testNodeWithAllocation(t, address, 10*ledger.SANBase)
	_ = identity
	apiServer := api.NewServer(node, config)
	server := httptest.NewServer(apiServer)
	defer server.Close()

	client := sdk.NewSanClient(server.URL, nil, time.Second)
	proof, err := client.AccountProof(address)
	if err != nil {
		t.Fatalf("AccountProof: %v", err)
	}
	root, _ := proof["root"].(string)
	if !sdk.VerifyAccountProof(proof, root) {
		t.Fatalf("proof did not verify against its own root")
	}
	if sdk.VerifyAccountProof(proof, "0xdead") {
		t.Fatalf("proof verified against the wrong root")
	}
}

func testNodeWithAllocation(t *testing.T, address string, units int64) (*netnode.Node, *ledger.NodeIdentity, netnode.NodeConfig) {
	t.Helper()
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	config := testConfig()
	config.GenesisAllocations = map[string]int64{address: units}
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node, identity, config
}
