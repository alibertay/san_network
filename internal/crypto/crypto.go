// Package crypto provides the post-quantum signature helper for SAN Network.
//
// The project signs/verifies with CRYSTALS-Dilithium2, standardized by NIST
// as ML-DSA-44 (FIPS 204). The Python implementation uses the pqcrypto
// bindings; this package uses the pure-Go CIRCL implementation. Both follow
// FIPS 204, so keys and signatures are interchangeable.
package crypto

import (
	"crypto/rand"
	"errors"

	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
)

const (
	PublicKeySize = 1312
	SecretKeySize = 2560
	SignatureSize = 2420
	BackendName   = "crypto/circl.mldsa44"
	AlgorithmName = "ML-DSA-44"
	PythonBackend = "pqcrypto.sign.ml_dsa_44"
)

var ErrUnavailable = errors.New("no post-quantum signature backend available")

// GenerateKeypair returns (publicKey, secretKey).
func GenerateKeypair() ([]byte, []byte, error) {
	publicKey, privateKey, err := mldsa44.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return publicKey.Bytes(), privateKey.Bytes(), nil
}

// Sign signs message with the secret key (randomized FIPS 204 signing).
func Sign(message, secretKey []byte) ([]byte, error) {
	if len(secretKey) != SecretKeySize {
		return nil, errors.New("invalid ML-DSA-44 secret key size")
	}
	var key mldsa44.PrivateKey
	if err := key.UnmarshalBinary(secretKey); err != nil {
		return nil, err
	}
	signature, err := key.Sign(rand.Reader, message, nil)
	if err != nil {
		return nil, err
	}
	return signature, nil
}

// Verify never panics for a bad signature, it simply returns false.
func Verify(message, signature, publicKey []byte) bool {
	if len(publicKey) != PublicKeySize || len(signature) != SignatureSize {
		return false
	}
	var key mldsa44.PublicKey
	if err := key.UnmarshalBinary(publicKey); err != nil {
		return false
	}
	return mldsa44.Verify(&key, message, nil, signature)
}
