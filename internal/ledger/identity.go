package ledger

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alibertay/san_network/internal/crypto"
)

// Node identity and post-quantum key management. Keys are resolved in this
// order:
//
//  1. an explicit NodeIdentity passed to the node (embedders/tests)
//  2. SAN_KEY_FILE / NodeConfig.key_file (JSON, see Save)
//  3. PRIVATE_KEY / PUBLIC_KEY environment variables (hex)
//  4. a freshly generated ephemeral key (not persisted)

// IdentityError marks an invalid or unusable node identity.
type IdentityError struct{ message string }

func (e *IdentityError) Error() string { return e.message }

func identityErrorf(format string, args ...any) error {
	return &IdentityError{message: fmt.Sprintf(format, args...)}
}

// NodeIdentity holds an optional ML-DSA-44 key pair.
type NodeIdentity struct {
	PrivateKey []byte
	PublicKey  []byte
}

// NewIdentity validates key sizes.
func NewIdentity(privateKey, publicKey []byte) (*NodeIdentity, error) {
	if privateKey != nil && len(privateKey) != crypto.SecretKeySize {
		return nil, identityErrorf(
			"private_key must be %d bytes for the loaded backend (%s); got %d",
			crypto.SecretKeySize, crypto.BackendName, len(privateKey))
	}
	if publicKey != nil && len(publicKey) != crypto.PublicKeySize {
		return nil, identityErrorf(
			"public_key must be %d bytes for the loaded backend (%s); got %d",
			crypto.PublicKeySize, crypto.BackendName, len(publicKey))
	}
	return &NodeIdentity{PrivateKey: privateKey, PublicKey: publicKey}, nil
}

// GenerateIdentity creates a fresh key pair.
func GenerateIdentity() (*NodeIdentity, error) {
	publicKey, privateKey, err := crypto.GenerateKeypair()
	if err != nil {
		return nil, err
	}
	return &NodeIdentity{PrivateKey: privateKey, PublicKey: publicKey}, nil
}

// IdentityFromEnv reads PRIVATE_KEY / PUBLIC_KEY.
func IdentityFromEnv(env map[string]string) (*NodeIdentity, error) {
	if env == nil {
		env = environmentMap()
	}
	privateHex := strings.TrimSpace(env["PRIVATE_KEY"])
	publicHex := strings.TrimSpace(env["PUBLIC_KEY"])
	if privateHex == "" && publicHex == "" {
		return nil, nil
	}
	var privateKey, publicKey []byte
	var err error
	if privateHex != "" {
		privateKey, err = hex.DecodeString(privateHex)
		if err != nil {
			return nil, identityErrorf("PRIVATE_KEY/PUBLIC_KEY must be hex encoded")
		}
	}
	if publicHex != "" {
		publicKey, err = hex.DecodeString(publicHex)
		if err != nil {
			return nil, identityErrorf("PRIVATE_KEY/PUBLIC_KEY must be hex encoded")
		}
	}
	return NewIdentity(privateKey, publicKey)
}

// IdentityFromFile loads a JSON key file.
func IdentityFromFile(path string) (*NodeIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, identityErrorf("Cannot read key file %s: %v", path, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, identityErrorf("Invalid key file %s: %v", path, err)
	}
	if payload == nil {
		return nil, identityErrorf("Invalid key file %s: expected a JSON object", path)
	}

	privateKey, err := decodeOptionalHex(payload["private_key"])
	if err != nil {
		return nil, identityErrorf("Invalid key file %s: keys must be hex", path)
	}
	publicKey, err := decodeOptionalHex(payload["public_key"])
	if err != nil {
		return nil, identityErrorf("Invalid key file %s: keys must be hex", path)
	}
	if privateKey == nil && publicKey == nil {
		return nil, identityErrorf("Key file %s contains no keys", path)
	}
	return NewIdentity(privateKey, publicKey)
}

// LoadIdentity resolves the node identity with a well-defined precedence.
func LoadIdentity(keyFile string, allowGenerate bool) (*NodeIdentity, error) {
	if keyFile != "" {
		if _, err := os.Stat(keyFile); err == nil {
			return IdentityFromFile(keyFile)
		}
		if !allowGenerate {
			return nil, identityErrorf("Key file %s does not exist", keyFile)
		}
		identity, err := GenerateIdentity()
		if err != nil {
			return nil, err
		}
		if err := identity.Save(keyFile); err != nil {
			return nil, err
		}
		return identity, nil
	}

	envIdentity, err := IdentityFromEnv(nil)
	if err != nil {
		return nil, err
	}
	if envIdentity != nil {
		return envIdentity, nil
	}

	if !allowGenerate {
		return nil, identityErrorf(
			"No node identity configured (set SAN_KEY_FILE or PRIVATE_KEY/PUBLIC_KEY)")
	}
	return GenerateIdentity()
}

// Save writes the key file with 0600 permissions.
func (identity *NodeIdentity) Save(path string) error {
	payload := struct {
		PrivateKey *string `json:"private_key"`
		PublicKey  *string `json:"public_key"`
		Algorithm  string  `json:"algorithm"`
	}{
		Algorithm: crypto.BackendName,
	}
	if identity.PrivateKey != nil {
		text := hex.EncodeToString(identity.PrivateKey)
		payload.PrivateKey = &text
	}
	if identity.PublicKey != nil {
		text := hex.EncodeToString(identity.PublicKey)
		payload.PublicKey = &text
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if absolute, err := filepath.Abs(path); err == nil {
		directory = filepath.Dir(absolute)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// PublicKeyHex returns the hex public key ("" when missing).
func (identity *NodeIdentity) PublicKeyHex() string {
	if identity.PublicKey == nil {
		return ""
	}
	return hex.EncodeToString(identity.PublicKey)
}

// CanSign reports whether a private key is available.
func (identity *NodeIdentity) CanSign() bool {
	return identity.PrivateKey != nil
}

// Sign returns the signature or nil when there is no private key.
func (identity *NodeIdentity) Sign(message []byte) []byte {
	if identity.PrivateKey == nil {
		return nil
	}
	signature, err := crypto.Sign(message, identity.PrivateKey)
	if err != nil {
		return nil
	}
	return signature
}

// SignHex returns the hex signature or "" when there is no private key.
func (identity *NodeIdentity) SignHex(message []byte) string {
	signature := identity.Sign(message)
	if signature == nil {
		return ""
	}
	return hex.EncodeToString(signature)
}

// VerifyIdentity verifies a hex signature against a hex public key.
func VerifyIdentity(message []byte, signatureHex, publicKeyHex string) bool {
	if signatureHex == "" || publicKeyHex == "" {
		return false
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	publicKey, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return false
	}
	return crypto.Verify(message, signature, publicKey)
}

func decodeOptionalHex(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return nil, nil
	}
	return hex.DecodeString(text)
}

func environmentMap() map[string]string {
	result := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			result[key] = value
		}
	}
	return result
}
