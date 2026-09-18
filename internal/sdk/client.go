// Package sdk is the Go port of sdk/client.py: a minimal SAN Network client
// for wallets and tools. Transactions are serialized and signed through
// internal/ledger, so payloads are byte-for-byte compatible with the Python
// node and SDK.
package sdk

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/ledger"
)

// DefaultTimeout mirrors SanClient's Python default (10 seconds).
const DefaultTimeout = 10 * time.Second

// DefaultDeployGasLimit and DefaultCallGasLimit mirror the Python SDK
// defaults for deploy_contract/call_contract.
const (
	DefaultDeployGasLimit = int64(2_000_000)
	DefaultCallGasLimit   = int64(1_000_000)
)

// SanClientError is raised when the node rejects a request.
type SanClientError struct{ Message string }

func (e *SanClientError) Error() string { return e.Message }

func clientErrorf(format string, args ...any) *SanClientError {
	return &SanClientError{Message: fmt.Sprintf(format, args...)}
}

// SanClient wraps the node REST API.
type SanClient struct {
	BaseURL  string
	Identity *ledger.NodeIdentity
	Timeout  time.Duration
	// Token is sent as "Authorization: Bearer <token>" when non-empty
	// (SAN_API_TOKEN on the node).
	Token string

	httpClient   *http.Client
	chainID      string
	chainIDKnown bool
}

// NewSanClient builds a client; the timeout defaults to 10s.
func NewSanClient(baseURL string, identity *ledger.NodeIdentity, timeout ...time.Duration) *SanClient {
	effective := DefaultTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		effective = timeout[0]
	}
	return &SanClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Identity:   identity,
		Timeout:    effective,
		httpClient: &http.Client{Timeout: effective},
	}
}

// SetToken configures the bearer token and returns the client for chaining.
func (c *SanClient) SetToken(token string) *SanClient {
	c.Token = strings.TrimSpace(token)
	return c
}

// SetTLS makes the client trust caFile (a PEM bundle) for HTTPS endpoints. An
// empty caFile uses the system trust store; insecureSkipVerify is meant for
// the launcher talking to its own self-signed devnet node.
func (c *SanClient) SetTLS(caFile string, insecureSkipVerify bool) error {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureSkipVerify}
	if strings.TrimSpace(caFile) != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("cannot read TLS CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("TLS CA %s contains no usable certificate", caFile)
		}
		config.RootCAs = pool
	}
	c.httpClient = &http.Client{
		Timeout:   c.Timeout,
		Transport: &http.Transport{TLSClientConfig: config},
	}
	return nil
}

// ---------------------------------------------------------------------- #
// Transport
// ---------------------------------------------------------------------- #

func (c *SanClient) client() *http.Client {
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: c.Timeout}
	}
	return c.httpClient
}

// authorize adds the bearer token to a request when configured.
func (c *SanClient) authorize(request *http.Request) {
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func (c *SanClient) get(path string, params url.Values) (any, error) {
	target := c.BaseURL + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, clientErrorf("GET %s failed: %v", path, err)
	}
	c.authorize(request)
	response, err := c.client().Do(request)
	if err != nil {
		return nil, clientErrorf("GET %s failed: %v", path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, clientErrorf("GET %s failed: %v", path, err)
	}
	if response.StatusCode >= 400 {
		return nil, clientErrorf("GET %s failed: %s", path, string(raw))
	}
	decoded, err := canonical.Decode(raw)
	if err != nil {
		return nil, clientErrorf("GET %s failed: %v", path, err)
	}
	return decoded, nil
}

func (c *SanClient) post(path string, payload any) (any, error) {
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return nil, clientErrorf("POST %s failed: %v", path, err)
	}
	request, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, clientErrorf("POST %s failed: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.client().Do(request)
	if err != nil {
		return nil, clientErrorf("POST %s failed: %v", path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, clientErrorf("POST %s failed: %v", path, err)
	}
	var body any
	decoded, decodeErr := canonical.Decode(raw)
	if decodeErr != nil {
		body = map[string]any{"raw": string(raw)}
	} else {
		body = decoded
	}
	if response.StatusCode >= 400 {
		return nil, clientErrorf("POST %s failed: %s", path, pythonRepr(body))
	}
	return body, nil
}

func (c *SanClient) getObject(path string) (map[string]any, error) {
	value, err := c.get(path, nil)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, clientErrorf("GET %s failed: unexpected response type %T", path, value)
	}
	return object, nil
}

func (c *SanClient) postObject(path string, payload any) (map[string]any, error) {
	value, err := c.post(path, payload)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, clientErrorf("POST %s failed: unexpected response type %T", path, value)
	}
	return object, nil
}

// ---------------------------------------------------------------------- #
// Queries
// ---------------------------------------------------------------------- #

// ChainID resolves (and caches) the node's chain id from /health.
func (c *SanClient) ChainID() (string, error) {
	if c.chainIDKnown {
		return c.chainID, nil
	}
	health, err := c.Health()
	if err != nil {
		return "", err
	}
	value := health["chain_id"]
	if value == nil {
		c.chainID = "None"
	} else {
		c.chainID = fmt.Sprintf("%v", value)
	}
	c.chainIDKnown = true
	return c.chainID, nil
}

func (c *SanClient) requireIdentity() (*ledger.NodeIdentity, error) {
	if c.Identity == nil || c.Identity.PublicKeyHex() == "" {
		return nil, &SanClientError{Message: "This client has no signing identity"}
	}
	return c.Identity, nil
}

// Address derives the account address of the signing identity.
func (c *SanClient) Address() (string, error) {
	identity, err := c.requireIdentity()
	if err != nil {
		return "", err
	}
	return ledger.AddressFromPublicKey(identity.PublicKeyHex())
}

func (c *SanClient) Genesis() (map[string]any, error) { return c.getObject("/genesis") }

func (c *SanClient) Health() (map[string]any, error) { return c.getObject("/health") }

func (c *SanClient) Finality() (map[string]any, error) { return c.getObject("/finality") }

func (c *SanClient) Validators() (map[string]any, error) { return c.getObject("/validators") }

// Account normalizes the address then queries /account/{address}.
func (c *SanClient) Account(address string) (map[string]any, error) {
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	return c.getObject("/account/" + normalized)
}

// Nonce returns the account nonce ("" means the client's own address).
func (c *SanClient) Nonce(address string) (int64, error) {
	if address == "" {
		own, err := c.Address()
		if err != nil {
			return 0, err
		}
		address = own
	}
	account, err := c.Account(address)
	if err != nil {
		return 0, err
	}
	return toInt64(account["nonce"]), nil
}

// BaseFee returns the current base fee (Python: int(health.get("base_fee", 1) or 1)).
func (c *SanClient) BaseFee() (int64, error) {
	health, err := c.Health()
	if err != nil {
		return 0, err
	}
	value := toInt64(health["base_fee"])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func (c *SanClient) Mempool() (map[string]any, error) { return c.getObject("/mempool") }

// Contracts returns the sorted deployed contract ids.
func (c *SanClient) Contracts() ([]string, error) {
	object, err := c.getObject("/contracts")
	if err != nil {
		return nil, err
	}
	raw, _ := object["contracts"].([]any)
	contracts := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			contracts = append(contracts, text)
		}
	}
	return contracts, nil
}

// ContractQuery executes a read-only contract call.
func (c *SanClient) ContractQuery(contractID, functionName string, params []any) (any, error) {
	if params == nil {
		params = []any{}
	}
	object, err := c.postObject("/contract/query", map[string]any{
		"contract_id":   contractID,
		"function_name": functionName,
		"params":        params,
	})
	if err != nil {
		return nil, err
	}
	return object["result"], nil
}

func (c *SanClient) Receipt(blockIndex, txIndex int64) (map[string]any, error) {
	return c.getObject(fmt.Sprintf("/receipt/%d/%d", blockIndex, txIndex))
}

// Transaction returns a transaction, its block and its receipt.
func (c *SanClient) Transaction(txID string) (map[string]any, error) {
	return c.getObject("/tx/" + txID)
}

func (c *SanClient) ReceiptForTx(txID string) (map[string]any, error) {
	return c.getObject("/receipt/tx/" + txID)
}

// MetricsText returns the raw Prometheus exposition.
func (c *SanClient) MetricsText() (string, error) {
	request, err := http.NewRequest(http.MethodGet, c.BaseURL+"/metrics", nil)
	if err != nil {
		return "", clientErrorf("GET /metrics failed: %v", err)
	}
	c.authorize(request)
	response, err := c.client().Do(request)
	if err != nil {
		return "", clientErrorf("GET /metrics failed: %v", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return "", clientErrorf("GET /metrics failed: %v", err)
	}
	if response.StatusCode >= 400 {
		return "", clientErrorf("GET /metrics failed: %s", string(raw))
	}
	return string(raw), nil
}

// AccountProof returns the Merkle inclusion proof of an account.
func (c *SanClient) AccountProof(address string) (map[string]any, error) {
	if address == "" {
		own, err := c.Address()
		if err != nil {
			return nil, err
		}
		address = own
	}
	normalized, err := ledger.NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	return c.getObject("/proof/account/" + normalized)
}

// VerifyAccountProof verifies a Merkle proof locally; expectedRoot "" means
// no root check (Python's expected_root=None).
func VerifyAccountProof(proof map[string]any, expectedRoot string) bool {
	if proof == nil {
		return false
	}
	root, _ := proof["root"].(string)
	if expectedRoot != "" && root != expectedRoot {
		return false
	}
	steps := proofSteps(proof["proof"])
	return ledger.VerifyMerkleProof(root, proof["leaf"], steps, int(toInt64(proof["index"])))
}

func proofSteps(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	steps := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		step, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		steps = append(steps, step)
	}
	return steps
}

// ---------------------------------------------------------------------- #
// Transactions
// ---------------------------------------------------------------------- #

// Sign adds the transaction signature to a payload in place.
func (c *SanClient) Sign(payload map[string]any) (map[string]any, error) {
	identity, err := c.requireIdentity()
	if err != nil {
		return nil, err
	}
	if identity.PrivateKey == nil {
		return nil, &SanClientError{Message: "This identity has no private key to sign with"}
	}
	signature, err := ledger.SignPayload(payload, identity.PrivateKey)
	if err != nil {
		return nil, &SanClientError{Message: err.Error()}
	}
	payload["signature"] = signature
	return payload, nil
}

// Send signs and submits a transaction; the nonce is filled in when nil.
func (c *SanClient) Send(nonce *int64, fields map[string]any) (map[string]any, error) {
	identity, err := c.requireIdentity()
	if err != nil {
		return nil, err
	}
	chainID, err := c.ChainID()
	if err != nil {
		return nil, err
	}
	effective := int64(0)
	if nonce == nil {
		effective, err = c.Nonce("")
		if err != nil {
			return nil, err
		}
	} else {
		effective = *nonce
	}
	payload := map[string]any{
		"chain_id": chainID,
		"sender":   identity.PublicKeyHex(),
		"nonce":    effective,
	}
	for key, value := range fields {
		payload[key] = value
	}
	signed, err := c.Sign(payload)
	if err != nil {
		return nil, err
	}
	return c.postObject("/transaction", signed)
}

// Transfer sends SAN to an address.
func (c *SanClient) Transfer(to string, valueSAN any, nonce *int64) (map[string]any, error) {
	receiver, err := ledger.NormalizeAddress(to)
	if err != nil {
		return nil, err
	}
	return c.Send(nonce, map[string]any{"receiver": receiver, "value": valueSAN})
}

// Faucet asks the node's faucet endpoint to fund an address. amountSAN nil
// uses the node's default faucet amount.
func (c *SanClient) Faucet(address string, amountSAN any) (map[string]any, error) {
	payload := map[string]any{"address": address}
	if amountSAN != nil {
		payload["amount"] = amountSAN
	}
	return c.postObject("/faucet", payload)
}

// DepositStake submits a validator deposit.
func (c *SanClient) DepositStake(amountSAN any, nonce *int64) (map[string]any, error) {
	units, err := ledger.SanToUnits(amountSAN)
	if err != nil {
		return nil, err
	}
	return c.Send(nonce, map[string]any{"validator": map[string]any{
		"command": "deposit",
		"amount":  units,
	}})
}

// Undelegate starts the unbonding period.
func (c *SanClient) Undelegate(nonce *int64) (map[string]any, error) {
	return c.Send(nonce, map[string]any{"validator": map[string]any{"command": "undelegate"}})
}

// WithdrawStake withdraws a released stake.
func (c *SanClient) WithdrawStake(nonce *int64) (map[string]any, error) {
	return c.Send(nonce, map[string]any{"validator": map[string]any{"command": "withdraw"}})
}

// DeployContract deploys PENA source; gasPrice nil means the current base fee.
func (c *SanClient) DeployContract(contractID, source string, gasLimit int64, gasPrice *int64, nonce *int64) (map[string]any, error) {
	price := int64(0)
	if gasPrice == nil {
		effective, err := c.BaseFee()
		if err != nil {
			return nil, err
		}
		price = effective
	} else {
		price = *gasPrice
	}
	return c.Send(nonce, map[string]any{
		"gas_limit": gasLimit,
		"gas_price": price,
		"contract_code": map[string]any{
			"command":     "deploy",
			"contract_id": contractID,
			"pena_code":   source,
		},
	})
}

// CallContract runs a contract function; gasPrice nil means the current base fee.
func (c *SanClient) CallContract(contractID, functionName string, params []any, gasLimit int64, gasPrice *int64, nonce *int64) (map[string]any, error) {
	price := int64(0)
	if gasPrice == nil {
		effective, err := c.BaseFee()
		if err != nil {
			return nil, err
		}
		price = effective
	} else {
		price = *gasPrice
	}
	if params == nil {
		params = []any{}
	}
	return c.Send(nonce, map[string]any{
		"gas_limit": gasLimit,
		"gas_price": price,
		"contract_code": map[string]any{
			"command":       "run",
			"contract_id":   contractID,
			"function_name": functionName,
			"params":        params,
		},
	})
}

// ---------------------------------------------------------------------- #
// Governance
// ---------------------------------------------------------------------- #

// GovernanceApproval signs a parameter-change approval bound to sender+nonce.
func (c *SanClient) GovernanceApproval(name string, value int64, nonce *int64) (map[string]any, error) {
	identity, err := c.requireIdentity()
	if err != nil {
		return nil, err
	}
	chainID, err := c.ChainID()
	if err != nil {
		return nil, err
	}
	effective := int64(0)
	if nonce == nil {
		effective, err = c.Nonce("")
		if err != nil {
			return nil, err
		}
	} else {
		effective = *nonce
	}
	message, err := canonical.Marshal(map[string]any{
		"chain_id":  chainID,
		"command":   "set_param",
		"name":      name,
		"value":     value,
		"tx_sender": identity.PublicKeyHex(),
		"tx_nonce":  effective,
	})
	if err != nil {
		return nil, &SanClientError{Message: err.Error()}
	}
	signature := identity.SignHex(message)
	if signature == "" {
		return nil, &SanClientError{Message: "This identity has no private key to sign with"}
	}
	return map[string]any{
		"public_key": identity.PublicKeyHex(),
		"signature":  signature,
	}, nil
}

// GovernanceSetParam submits a parameter change with its approvals.
func (c *SanClient) GovernanceSetParam(name string, value int64, approvals []any, nonce *int64) (map[string]any, error) {
	if approvals == nil {
		approvals = []any{}
	}
	return c.Send(nonce, map[string]any{"governance": map[string]any{
		"command":   "set_param",
		"name":      name,
		"value":     value,
		"approvals": approvals,
	}})
}

func toInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case string:
		var parsed int64
		_, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}
