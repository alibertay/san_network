package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/crypto"
)

// Transaction mirrors blockchain/Transaction.py. The payload is a Python-like
// dynamic JSON object (map[string]any) so canonical serialization is
// byte-for-byte identical to the reference implementation.

// Fields that are protocol metadata and are never part of a user signature.
var TransactionMetaFields = []string{"signature", "fee"}

// Payload keys that trigger code execution (and therefore need gas).
var executionFields = []string{"bytecode", "contract_code"}

var validatorCommands = map[string]bool{
	"deposit": true, "undelegate": true, "withdraw": true, "evidence": true,
}

var governanceCommands = map[string]bool{"set_param": true}

// Transaction is a normalized, fee-stamped payload.
type Transaction struct {
	Payload map[string]any
	Fee     int64
	Data    []byte
}

// NewTransaction normalizes, validates and fee-stamps a payload.
func NewTransaction(data any, feeRate *int64) (*Transaction, error) {
	payload, err := NormalizePayload(data)
	if err != nil {
		return nil, err
	}
	delete(payload, "fee")

	if !truthy(ChainIDOf(payload)) {
		return nil, fmt.Errorf("Transaction payload must include a non-empty 'chain_id'")
	}
	if message := ValidateGasFields(payload); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	if message := ValidateValidatorCommand(payload); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	if message := ValidateGovernanceCommand(payload); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	if message := ValidateContractCode(payload); message != "" {
		return nil, fmt.Errorf("%s", message)
	}

	rate := MinFeePerByte
	if feeRate != nil {
		rate = *feeRate
	}
	fee := ExpectedFee(payload, rate)
	payload["fee"] = fee
	encoded, err := canonical.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Transaction{Payload: payload, Fee: fee, Data: encoded}, nil
}

// NormalizePayload accepts a map, a JSON string or JSON bytes.
func NormalizePayload(data any) (map[string]any, error) {
	switch value := data.(type) {
	case map[string]any:
		copy := make(map[string]any, len(value))
		for key, item := range value {
			copy[key] = item
		}
		return copy, nil
	case []byte:
		return decodePayload(value)
	case string:
		return decodePayload([]byte(value))
	default:
		return nil, fmt.Errorf("Unsupported transaction payload type: %T", data)
	}
}

func decodePayload(raw []byte) (map[string]any, error) {
	decoded, err := canonical.Decode(raw)
	if err != nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Transaction payload must be a JSON object")
	}
	return object, nil
}

// ChainIDOf returns the chain binding field.
func ChainIDOf(payload map[string]any) any {
	return payload["chain_id"]
}

// HasExecution reports whether the payload triggers code execution.
func HasExecution(payload map[string]any) bool {
	for _, field := range executionFields {
		if _, ok := payload[field]; ok {
			return true
		}
	}
	return false
}

func intField(payload map[string]any, name string) (int64, error) {
	value, ok := payload[name]
	if !ok || value == nil {
		return 0, nil
	}
	switch number := value.(type) {
	case bool:
		return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
	case int:
		if number < 0 {
			return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
		}
		return int64(number), nil
	case int64:
		if number < 0 {
			return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
		}
		return number, nil
	case int32:
		if number < 0 {
			return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
		}
		return int64(number), nil
	case float64:
		if number < 0 {
			return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
		}
		return int64(number), nil
	case *big.Int:
		if number == nil || number.Sign() < 0 || !number.IsInt64() {
			return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
		}
		return number.Int64(), nil
	default:
		return 0, fmt.Errorf("'%s' must be a non-negative integer", name)
	}
}

// GasLimitOf returns the payload gas limit.
func GasLimitOf(payload map[string]any) int64 {
	value, _ := intField(payload, "gas_limit")
	return value
}

// GasPriceOf returns the payload gas price.
func GasPriceOf(payload map[string]any) int64 {
	value, _ := intField(payload, "gas_price")
	return value
}

// ValidateGasFields returns an error message or "".
func ValidateGasFields(payload map[string]any) string {
	gasLimit, err := intField(payload, "gas_limit")
	if err != nil {
		return err.Error()
	}
	gasPrice, err := intField(payload, "gas_price")
	if err != nil {
		return err.Error()
	}
	if HasExecution(payload) {
		if gasLimit < 1 {
			return "execution transactions require 'gas_limit' >= 1"
		}
		if gasPrice < MinGasPrice {
			return fmt.Sprintf("'gas_price' must be at least %d", MinGasPrice)
		}
		return ""
	}
	if gasLimit != 0 || gasPrice != 0 {
		return "transactions without execution must not set gas fields"
	}
	return ""
}

// ValidateValidatorCommand applies the structural rules for staking/slashing
// system transactions.
func ValidateValidatorCommand(payload map[string]any) string {
	command, ok := payload["validator"].(map[string]any)
	if !ok {
		if payload["validator"] == nil {
			return ""
		}
		return "'validator' must be an object"
	}
	name, _ := command["command"].(string)
	if !validatorCommands[name] {
		return fmt.Sprintf("unknown validator command: %v", command["command"])
	}
	if HasExecution(payload) {
		return "validator commands must not contain execution payloads"
	}
	if _, ok := payload["value"]; ok {
		return "validator commands must not be transfers"
	}
	if _, ok := payload["receiver"]; ok {
		return "validator commands must not be transfers"
	}

	if name == "deposit" {
		amount, ok := command["amount"].(int64)
		if !ok || amount <= 0 {
			return "validator deposit requires a positive integer 'amount'"
		}
	}
	if name == "evidence" {
		for _, field := range []string{"vote_a", "vote_b"} {
			vote, ok := command[field].(map[string]any)
			if !ok {
				return fmt.Sprintf("evidence requires a '%s' vote object", field)
			}
			if !truthy(vote["public_key"]) || !truthy(vote["signature"]) {
				return fmt.Sprintf("evidence '%s' must be a signed vote", field)
			}
		}
	}
	return ""
}

// ValidateGovernanceCommand applies the structural rules for on-chain
// parameter changes.
func ValidateGovernanceCommand(payload map[string]any) string {
	command, ok := payload["governance"].(map[string]any)
	if !ok {
		if payload["governance"] == nil {
			return ""
		}
		return "'governance' must be an object"
	}
	name, _ := command["command"].(string)
	if !governanceCommands[name] {
		return fmt.Sprintf("unknown governance command: %v", command["command"])
	}
	if HasExecution(payload) {
		return "governance commands must not contain execution payloads"
	}
	if _, ok := payload["value"]; ok {
		return "governance commands must not be transfers"
	}
	if _, ok := payload["receiver"]; ok {
		return "governance commands must not be transfers"
	}
	if payload["validator"] != nil {
		return "governance and validator commands cannot be combined"
	}
	if !truthy(command["name"]) {
		return "governance set_param requires a 'name'"
	}
	if _, ok := command["value"].(int64); !ok {
		return "governance set_param requires an integer 'value'"
	}
	approvals, ok := command["approvals"].([]any)
	if !ok || len(approvals) == 0 {
		return "governance set_param requires at least one approval"
	}
	for _, raw := range approvals {
		approval, ok := raw.(map[string]any)
		if !ok {
			return "governance approvals must be objects"
		}
		if !truthy(approval["public_key"]) || !truthy(approval["signature"]) {
			return "governance approvals must be signed by validators"
		}
	}
	return ""
}

// ValidateContractCode applies the structural rules for deploy/run payloads.
func ValidateContractCode(payload map[string]any) string {
	code, ok := payload["contract_code"].(map[string]any)
	if !ok {
		if payload["contract_code"] == nil {
			return ""
		}
		return "'contract_code' must be an object"
	}
	command, _ := code["command"].(string)
	if command != "deploy" && command != "run" {
		return fmt.Sprintf("unknown contract command: %v", code["command"])
	}
	contractID, ok := code["contract_id"].(string)
	if !ok || contractID == "" {
		return "contract requires a non-empty string 'contract_id'"
	}
	if command == "deploy" {
		_, hasPena := code["pena_code"].(string)
		_, hasBytecode := code["bytecode"].([]any)
		if !hasPena && !hasBytecode {
			return "deploy requires 'pena_code' or 'bytecode'"
		}
		return ""
	}
	functionName, ok := code["function_name"].(string)
	if !ok || functionName == "" {
		return "run requires a non-empty 'function_name'"
	}
	if params, ok := code["params"]; ok && params != nil {
		if _, ok := params.([]any); !ok {
			return "'params' must be a list"
		}
	}
	return ""
}

// SerializeMessage serializes the signed part of a payload (signature and fee
// are excluded).
func SerializeMessage(message map[string]any) ([]byte, error) {
	toSign := make(map[string]any, len(message))
	for key, value := range message {
		if key == "signature" || key == "fee" {
			continue
		}
		toSign[key] = value
	}
	return canonical.Marshal(toSign)
}

func feeBasis(payload map[string]any) ([]byte, error) {
	basis := make(map[string]any, len(payload))
	for key, value := range payload {
		if key == "fee" {
			continue
		}
		basis[key] = value
	}
	return canonical.Marshal(basis)
}

// ExpectedFee is the deterministic protocol fee: size fee plus maximum gas
// cost.
func ExpectedFee(payload map[string]any, feeRate int64) int64 {
	basis, err := feeBasis(payload)
	sizeFee := int64(0)
	if err == nil {
		sizeFee = int64(len(basis)) * feeRate
	}
	gasLimit, errLimit := intField(payload, "gas_limit")
	if errLimit != nil {
		gasLimit = 0
	}
	gasPrice, errPrice := intField(payload, "gas_price")
	if errPrice != nil {
		gasPrice = 0
	}
	return sizeFee + gasLimit*gasPrice
}

// TxID is the replay-protection id: hash of the signed part of the payload.
func TxID(payload map[string]any) string {
	message, err := SerializeMessage(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(message)
	return hex.EncodeToString(digest[:])
}

// SignPayload signs a payload and returns the hex signature.
func SignPayload(payload map[string]any, privateKey []byte) (string, error) {
	message, err := SerializeMessage(payload)
	if err != nil {
		return "", err
	}
	signature, err := crypto.Sign(message, privateKey)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(signature), nil
}

// VerifyTransaction verifies a transaction signature. Malformed or unsigned
// data returns false instead of panicking.
func VerifyTransaction(data any) bool {
	var tx map[string]any
	switch value := data.(type) {
	case []byte:
		decoded, err := canonical.Decode(value)
		if err != nil {
			return false
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return false
		}
		tx = object
	case string:
		decoded, err := canonical.Decode([]byte(value))
		if err != nil {
			return false
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return false
		}
		tx = object
	case map[string]any:
		tx = value
	default:
		return false
	}

	sender, senderOK := tx["sender"].(string)
	signatureHex, signatureOK := tx["signature"].(string)
	if !senderOK || !signatureOK {
		return false
	}
	publicKey, err := hex.DecodeString(sender)
	if err != nil {
		return false
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	message, err := SerializeMessage(tx)
	if err != nil {
		return false
	}
	return crypto.Verify(message, signature, publicKey)
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != ""
	case bool:
		return typed
	case int64:
		return typed != 0
	case int:
		return typed != 0
	case float64:
		return typed != 0
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}

// EncodePayload is the canonical JSON string of a payload (test helper).
func EncodePayload(payload map[string]any) (string, error) {
	data, err := canonical.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
