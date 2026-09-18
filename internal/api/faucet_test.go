package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/alibertay/san_network/internal/api"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

const faucetReceiver = "0x" + "1234567890abcdef1234567890abcdef12345678"

// TestFaucetDisabledByDefault verifies the endpoint is invisible unless
// SAN_FAUCET=1 was set.
func TestFaucetDisabledByDefault(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	server := api.NewServer(node, config)
	recorder := doRequest(t, server, http.MethodPost, "/faucet",
		`{"address":"`+faucetReceiver+`"}`, nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled faucet status: got %d, want 404 (%s)", recorder.Code, recorder.Body.String())
	}
	if detail, _ := decodeObject(t, recorder)["detail"].(string); !strings.Contains(detail, "disabled") {
		t.Errorf("disabled faucet detail: %v", detail)
	}
}

// TestFaucetRequiresToken verifies SAN_API_TOKEN is enforced with a 401.
func TestFaucetRequiresToken(t *testing.T) {
	node, _, config := newTestNode(t, nil)
	config.FaucetEnabled = true
	config.APIToken = "secret"
	server := api.NewServer(node, config)

	recorder := doRequest(t, server, http.MethodPost, "/faucet",
		`{"address":"`+faucetReceiver+`"}`, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized faucet status: got %d, want 401 (%s)", recorder.Code, recorder.Body.String())
	}
	if detail := decodeObject(t, recorder)["detail"]; detail != "Unauthorized" {
		t.Errorf("unauthorized detail: %v", detail)
	}
}

func fundedFaucetServer(t *testing.T) (*api.Server, *netnode.Node, *ledger.NodeIdentity) {
	t.Helper()
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	nodeAddress, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		t.Fatalf("AddressFromPublicKey: %v", err)
	}
	config := testConfig()
	config.GenesisAllocations = map[string]int64{nodeAddress: 1000 * ledger.SANBase}
	node, err := netnode.NewNode(config, identity)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	config.FaucetEnabled = true
	config.FaucetAmount = 10 * ledger.SANBase
	config.FaucetMax = 100 * ledger.SANBase
	config.FaucetCooldown = 60
	server := api.NewServer(node, config)
	return server, node, identity
}

// TestFaucetFundsAddress verifies the successful transfer: it is signed by the
// node identity, admitted through the normal mempool path and verifies with
// ledger.VerifyTransaction.
func TestFaucetFundsAddress(t *testing.T) {
	server, node, identity := fundedFaucetServer(t)
	body := `{"address":"` + faucetReceiver + `","amount":25}`
	recorder := doRequest(t, server, http.MethodPost, "/faucet", body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("faucet status: got %d (%s)", recorder.Code, recorder.Body.String())
	}
	result := decodeObject(t, recorder)
	if result["status"] != "pooled" {
		t.Errorf("faucet status field: %v", result["status"])
	}
	if result["amount"] != "25" {
		t.Errorf("faucet amount field: %v", result["amount"])
	}
	txID, _ := result["tx_id"].(string)
	if txID == "" {
		t.Fatalf("faucet response has no tx_id: %v", result)
	}

	record := node.GetTransaction(txID)
	if record == nil {
		t.Fatalf("transaction %s was not admitted by the node", txID)
	}
	transaction, _ := record["transaction"].(map[string]any)
	if transaction == nil {
		t.Fatalf("transaction payload missing: %v", record)
	}
	if !ledger.VerifyTransaction(transaction) {
		t.Fatalf("faucet transaction does not verify: %v", transaction)
	}
	if sender, _ := transaction["sender"].(string); sender != identity.PublicKeyHex() {
		t.Errorf("faucet sender: got %v, want the node identity", sender)
	}
	receiver, _ := transaction["receiver"].(string)
	if receiver != faucetReceiver {
		t.Errorf("faucet receiver: got %v", receiver)
	}
	account, err := node.GetAccount(faucetReceiver)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if balance := account["balance_units"]; balance != int64(25*ledger.SANBase) {
		t.Errorf("receiver balance: got %v, want 25 SAN", balance)
	}
}

// TestFaucetCooldownAndCaps covers the per-address cooldown and amount cap.
func TestFaucetCooldownAndCaps(t *testing.T) {
	server, _, _ := fundedFaucetServer(t)
	body := `{"address":"` + faucetReceiver + `"}`

	first := doRequest(t, server, http.MethodPost, "/faucet", body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first faucet status: got %d (%s)", first.Code, first.Body.String())
	}
	second := doRequest(t, server, http.MethodPost, "/faucet", body, nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second faucet status: got %d, want 429 (%s)", second.Code, second.Body.String())
	}
	if detail, _ := decodeObject(t, second)["detail"].(string); !strings.Contains(detail, "cooldown") {
		t.Errorf("cooldown detail: %v", detail)
	}

	other := "0x" + strings.Repeat("cd", 20)
	capped := doRequest(t, server, http.MethodPost, "/faucet",
		fmt.Sprintf(`{"address":%q,"amount":1000}`, other), nil)
	if capped.Code != http.StatusBadRequest {
		t.Fatalf("capped faucet status: got %d, want 400 (%s)", capped.Code, capped.Body.String())
	}
	if detail, _ := decodeObject(t, capped)["detail"].(string); !strings.Contains(detail, "maximum") {
		t.Errorf("capped detail: %v", detail)
	}

	invalid := doRequest(t, server, http.MethodPost, "/faucet", `{"address":"0xnope"}`, nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid address status: got %d, want 400", invalid.Code)
	}
}
