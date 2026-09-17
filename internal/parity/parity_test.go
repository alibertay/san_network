package parity_test

import (
	"encoding/hex"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/crypto"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/parity"
)

func fixture(t *testing.T) map[string]any {
	t.Helper()
	root, err := canonical.Decode(parity.Foundation())
	if err != nil {
		t.Fatalf("cannot decode foundation fixture: %v", err)
	}
	object, ok := root.(map[string]any)
	if !ok {
		t.Fatalf("foundation fixture is not an object")
	}
	return object
}

func mustString(t *testing.T, value any) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("expected string, got %T", value)
	}
	return text
}

func canonicalString(t *testing.T, value any) string {
	t.Helper()
	text, err := canonical.MarshalString(value)
	if err != nil {
		t.Fatalf("cannot marshal value: %v", err)
	}
	return text
}

func TestCanonicalParity(t *testing.T) {
	root := fixture(t)
	entries, ok := root["canonical"].([]any)
	if !ok {
		t.Fatalf("canonical fixture missing")
	}
	for index, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("canonical[%d] is not an object", index)
		}
		decoded, err := canonical.Decode([]byte(mustString(t, entry["input"])))
		if err != nil {
			t.Fatalf("canonical[%d] input does not decode: %v", index, err)
		}
		got := canonicalString(t, decoded)
		want := mustString(t, entry["expected"])
		if got != want {
			t.Errorf("canonical[%d]:\n got: %s\nwant: %s", index, got, want)
		}
	}
}

func TestFloatReprParity(t *testing.T) {
	root := fixture(t)
	entries, ok := root["float_repr"].([]any)
	if !ok {
		t.Fatalf("float_repr fixture missing")
	}
	for index, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("float_repr[%d] is not an object", index)
		}
		got := canonicalString(t, entry["value"])
		want := mustString(t, entry["expected"])
		if got != want {
			t.Errorf("float_repr[%d]: got %s, want %s", index, got, want)
		}
	}
}

func TestAddressParity(t *testing.T) {
	root := fixture(t)
	section, ok := root["address"].(map[string]any)
	if !ok {
		t.Fatalf("address fixture missing")
	}
	for index, raw := range section["valid"].([]any) {
		entry := raw.(map[string]any)
		publicKey := mustString(t, entry["public_key"])
		want := mustString(t, entry["address"])
		got, err := ledger.AddressFromPublicKey(publicKey)
		if err != nil {
			t.Fatalf("address[%d]: %v", index, err)
		}
		if got != want {
			t.Errorf("address[%d]: got %s, want %s", index, got, want)
		}
	}

	addresses := section["addresses"].(map[string]any)
	for _, raw := range addresses["valid"].([]any) {
		text := raw.(string)
		if !ledger.IsValidAddress(text) {
			t.Errorf("expected %q to be a valid address", text)
		}
		normalized, err := ledger.NormalizeAddress(text)
		if err != nil || normalized != stringLower(text) {
			t.Errorf("normalize %q: got %q, err %v", text, normalized, err)
		}
	}
	for _, raw := range addresses["invalid"].([]any) {
		value := raw
		if ledger.IsValidAddress(value) {
			t.Errorf("expected %v to be invalid", value)
		}
	}
}

func stringLower(text string) string {
	result := []rune(text)
	for i, r := range result {
		if r >= 'A' && r <= 'Z' {
			result[i] = r + ('a' - 'A')
		}
	}
	return string(result)
}

func TestMerkleParity(t *testing.T) {
	root := fixture(t)
	section, ok := root["merkle"].(map[string]any)
	if !ok {
		t.Fatalf("merkle fixture missing")
	}
	leaves := section["leaves"].([]any)

	for _, raw := range section["leaf_hashes"].([]any) {
		entry := raw.(map[string]any)
		got := ledger.LeafHash(entry["leaf"])
		want := mustString(t, entry["hash"])
		if got != want {
			t.Errorf("leaf hash mismatch for %v: got %s, want %s", entry["leaf"], got, want)
		}
	}

	if got, want := ledger.MerkleRoot(leaves), mustString(t, section["root"]); got != want {
		t.Errorf("merkle root: got %s, want %s", got, want)
	}
	if got, want := ledger.MerkleRoot(nil), mustString(t, section["empty_root"]); got != want {
		t.Errorf("empty root: got %s, want %s", got, want)
	}

	for _, raw := range section["proofs"].([]any) {
		entry := raw.(map[string]any)
		index := int(entry["index"].(int64))
		proof, err := ledger.MerkleProof(leaves, index)
		if err != nil {
			t.Fatalf("proof[%d]: %v", index, err)
		}
		got := canonicalString(t, proof)
		want := canonicalString(t, entry["proof"])
		if got != want {
			t.Errorf("proof[%d]:\n got: %s\nwant: %s", index, got, want)
		}
		if !ledger.VerifyMerkleProof(ledger.MerkleRoot(leaves), leaves[index], proof, index) {
			t.Errorf("proof[%d] does not verify", index)
		}
	}
}

func TestStateParity(t *testing.T) {
	root := fixture(t)
	section, ok := root["state"].(map[string]any)
	if !ok {
		t.Fatalf("state fixture missing")
	}
	input := buildStateInput(t, section["input"].(map[string]any))

	gotEntries := canonicalString(t, ledger.StateEntries(input))
	wantEntries := canonicalString(t, section["entries"])
	if gotEntries != wantEntries {
		t.Errorf("state entries mismatch:\n got: %s\nwant: %s", gotEntries, wantEntries)
	}
	if got, want := ledger.StateRoot(input), mustString(t, section["root"]); got != want {
		t.Errorf("state root: got %s, want %s", got, want)
	}

	accountProof := section["account_proof"].(map[string]any)
	key := mustString(t, accountProof["key"])
	gotProof := ledger.StateEntryProof(input, key)
	if gotProof == nil {
		t.Fatalf("missing account proof for %s", key)
	}
	got := canonicalString(t, gotProof)
	want := canonicalString(t, accountProof)
	if got != want {
		t.Errorf("state proof mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func buildStateInput(t *testing.T, raw map[string]any) ledger.StateInput {
	t.Helper()
	input := ledger.StateInput{
		Balances:     map[string]int64{},
		Nonces:       map[string]int64{},
		Validators:   map[string]map[string]any{},
		TotalSlashed: raw["total_slashed"].(int64),
		Storage:      raw["storage"].(map[string]any),
		Parameters:   map[string]int64{},
		BaseFee:      raw["base_fee"].(int64),
		TotalBurned:  raw["total_burned"].(int64),
	}
	for address, value := range raw["balances"].(map[string]any) {
		input.Balances[address] = value.(int64)
	}
	for address, value := range raw["nonces"].(map[string]any) {
		input.Nonces[address] = value.(int64)
	}
	for address, value := range raw["validators"].(map[string]any) {
		input.Validators[address] = value.(map[string]any)
	}
	if parameters, ok := raw["parameters"].(map[string]any); ok {
		for name, value := range parameters {
			input.Parameters[name] = value.(int64)
		}
	}
	return input
}

func TestEconomicsParity(t *testing.T) {
	root := fixture(t)
	section, ok := root["economics"].(map[string]any)
	if !ok {
		t.Fatalf("economics fixture missing")
	}
	for _, raw := range section["san_to_units"].([]any) {
		entry := raw.(map[string]any)
		got, err := ledger.SanToUnits(entry["amount"])
		if err != nil {
			t.Fatalf("san_to_units(%v): %v", entry["amount"], err)
		}
		want := entry["units"].(int64)
		if got != want {
			t.Errorf("san_to_units(%v): got %d, want %d", entry["amount"], got, want)
		}
	}
	for _, raw := range section["invalid_amounts"].([]any) {
		if _, err := ledger.SanToUnits(raw); err == nil {
			t.Errorf("san_to_units(%v): expected error", raw)
		}
	}
	for _, raw := range section["units_to_san"].([]any) {
		entry := raw.(map[string]any)
		got := ledger.UnitsToSAN(entry["units"].(int64))
		want := mustString(t, entry["text"])
		if got != want {
			t.Errorf("units_to_san(%v): got %s, want %s", entry["units"], got, want)
		}
	}
	for _, raw := range section["fee_rate"].([]any) {
		entry := raw.(map[string]any)
		got := ledger.FeeRateForTransactionCount(entry["count"].(int64))
		want := entry["rate"].(int64)
		if got != want {
			t.Errorf("fee_rate(%v): got %d, want %d", entry["count"], got, want)
		}
	}
	for _, raw := range section["next_base_fee"].([]any) {
		entry := raw.(map[string]any)
		got := ledger.NextBaseFee(entry["current"].(int64), entry["used"].(int64), entry["limit"].(int64))
		want := entry["fee"].(int64)
		if got != want {
			t.Errorf("next_base_fee(%v): got %d, want %d", entry, got, want)
		}
	}
}

func TestMLDSAParity(t *testing.T) {
	root := fixture(t)
	section, ok := root["identity"].(map[string]any)
	if !ok {
		t.Fatalf("identity fixture missing")
	}
	privateKey := mustHex(t, section["private_key_hex"])
	publicKey := mustHex(t, section["public_key_hex"])
	message := mustHex(t, section["message_hex"])
	signature := mustHex(t, section["signature_hex"])

	if len(publicKey) != crypto.PublicKeySize || len(privateKey) != crypto.SecretKeySize {
		t.Fatalf("unexpected key sizes: %d / %d", len(publicKey), len(privateKey))
	}
	if !crypto.Verify(message, signature, publicKey) {
		t.Fatalf("Python ML-DSA signature rejected by Go implementation")
	}
	if crypto.Verify([]byte("tampered"), signature, publicKey) {
		t.Fatalf("tampered message verified")
	}

	identity, err := ledger.NewIdentity(privateKey, publicKey)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	roundtrip := identity.Sign(message)
	if roundtrip == nil || !crypto.Verify(message, roundtrip, publicKey) {
		t.Fatalf("Go signature roundtrip failed")
	}
}

func mustHex(t *testing.T, value any) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(mustString(t, value))
	if err != nil {
		t.Fatalf("invalid hex: %v", err)
	}
	return decoded
}
