package ledger

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Account address derivation:
//
//	address = "0x" + sha3_256(public_key)[:20].hex()
//
// A public key (1312 bytes for ML-DSA-44) is far too large to be a ledger
// key, so the account address is the truncated hash of the public key.
const AddressBytes = 20

var addressRegex = regexp.MustCompile(`^0x[0-9a-f]{40}$`)

// AddressError marks an invalid address or public key.
type AddressError struct{ message string }

func (e *AddressError) Error() string { return e.message }

func newAddressError(format string, args ...any) error {
	return &AddressError{message: fmt.Sprintf(format, args...)}
}

// AddressFromPublicKey derives the canonical 0x address from a public key
// ([]byte or hex string).
func AddressFromPublicKey(publicKey any) (string, error) {
	var keyBytes []byte
	switch value := publicKey.(type) {
	case string:
		raw := strings.TrimSpace(value)
		raw = strings.TrimPrefix(strings.TrimPrefix(raw, "0x"), "0X")
		raw = strings.Join(strings.Fields(raw), "")
		decoded, err := hex.DecodeString(raw)
		if err != nil {
			return "", newAddressError("Public key is not hex: %v", publicKey)
		}
		keyBytes = decoded
	case []byte:
		keyBytes = value
	default:
		if publicKey == nil {
			return "", newAddressError("Unsupported public key type: %T", publicKey)
		}
		return "", newAddressError("Unsupported public key type: %T", publicKey)
	}

	if len(keyBytes) == 0 {
		return "", newAddressError("Public key is empty")
	}

	digest := sha3.Sum256(keyBytes)
	return "0x" + hex.EncodeToString(digest[:AddressBytes]), nil
}

// IsValidAddress reports whether value is a 0x-prefixed 40-hex-char address.
func IsValidAddress(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	return addressRegex.MatchString(strings.ToLower(strings.TrimSpace(text)))
}

// NormalizeAddress validates and returns the lowercase 0x form.
func NormalizeAddress(value any) (string, error) {
	if !IsValidAddress(value) {
		return "", newAddressError("Invalid address: %v", value)
	}
	return strings.ToLower(strings.TrimSpace(value.(string))), nil
}

// TryAddressFromPublicKey returns the address or ok=false on failure.
func TryAddressFromPublicKey(publicKey any) (string, bool) {
	address, err := AddressFromPublicKey(publicKey)
	if err != nil {
		return "", false
	}
	return address, true
}
