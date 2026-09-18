package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/crypto"
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

func TestIdentityRejectsMismatchedKeys(t *testing.T) {
	publicKey, privateKey, err := crypto.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	otherPublicKey, _, err := crypto.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if _, err := NewIdentity(privateKey, otherPublicKey); err == nil {
		t.Fatalf("NewIdentity accepted a private key with a foreign public key")
	}
	if _, err := NewIdentity(privateKey, publicKey); err != nil {
		t.Fatalf("NewIdentity rejected the matching pair: %v", err)
	}
}

func TestIdentityFromFileRejectsMismatchedKeys(t *testing.T) {
	publicKey, privateKey, err := crypto.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	otherPublicKey, _, err := crypto.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	path := filepath.Join(t.TempDir(), "san_key.json")
	payload, err := json.Marshal(map[string]any{
		"private_key": hex.EncodeToString(privateKey),
		"public_key":  hex.EncodeToString(otherPublicKey),
		"algorithm":   crypto.BackendName,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := IdentityFromFile(path); err == nil {
		t.Fatalf("IdentityFromFile accepted mismatched keys")
	}

	payload, err = json.Marshal(map[string]any{
		"private_key": hex.EncodeToString(privateKey),
		"public_key":  hex.EncodeToString(publicKey),
		"algorithm":   crypto.BackendName,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := IdentityFromFile(path); err != nil {
		t.Fatalf("IdentityFromFile rejected the matching pair: %v", err)
	}
}

func TestIdentitySaveAndLoadRoundTrip(t *testing.T) {
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "nested", "san_key.json")
	if err := identity.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := IdentityFromFile(path)
	if err != nil {
		t.Fatalf("IdentityFromFile: %v", err)
	}
	if loaded.PublicKeyHex() != identity.PublicKeyHex() {
		t.Fatalf("public key mismatch after round trip")
	}
	if !crypto.KeysMatch(loaded.PrivateKey, loaded.PublicKey) {
		t.Fatalf("loaded key pair does not match")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key file permissions: got %o, want 600", info.Mode().Perm())
		}
	}
}

func TestBlockFromDictNeverPanicsOnHostileInput(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{"index": "nan"},
		{"index": int64(1), "timestamp": "not-a-number"},
		{"index": int64(1), "timestamp": map[string]any{"nested": true}},
		{"index": int64(1), "transactions": "not-a-list"},
		{"index": int64(1), "transactions": []any{nil, 1, "x"}},
		{"index": int64(1), "validator": []any{1, 2}, "validator_signature": map[string]any{}},
		{"index": int64(1), "version": int64(999)},
		{"index": int64(1), "round": -5, "state_root": []any{}},
	}
	for index, data := range cases {
		guardNoPanic(t, "BlockFromDict", func() { _, _ = BlockFromDict(data) })
		_ = index
	}
}

func TestVerifyTransactionNeverPanicsOnHostileInput(t *testing.T) {
	cases := []any{
		nil,
		int64(1),
		[]byte(""),
		"not json",
		map[string]any{},
		map[string]any{"sender": int64(1), "signature": "00"},
		map[string]any{"sender": "zz", "signature": "zz"},
		map[string]any{"sender": "aa", "signature": make([]byte, crypto.SignatureSize)},
		map[string]any{"sender": "aa", "signature": "00", "chain_id": map[string]any{}},
	}
	for index, data := range cases {
		guardNoPanic(t, "VerifyTransaction", func() { _ = VerifyTransaction(data) })
		_ = index
	}
}

func FuzzBlockFromDict(f *testing.F) {
	seeds := []string{
		`{"index":0,"timestamp":0.0,"transactions":[]}`,
		`{"index":1,"timestamp":"x"}`,
		`{"index":1,"transactions":[{"sender":"aa"}]}`,
		`{"index":-1,"round":-1}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := canonical.Decode(data)
		if err != nil {
			return
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("BlockFromDict(%q) panicked: %v", data, recovered)
			}
		}()
		_, _ = BlockFromDict(object)
	})
}
