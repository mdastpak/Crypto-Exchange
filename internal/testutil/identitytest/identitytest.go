// Package identitytest provides a field sealer for tests of national IDs,
// without data-protection keys in the database.
package identitytest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

// Sealer seals fields with AES-256-GCM and one random key, as
// keys.Keyring.SealField does with a data key.
type Sealer struct{ aead cipher.AEAD }

// NewSealer returns a Sealer with a new random key.
func NewSealer() *Sealer {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Sealer{aead: aead}
}

// SealField encrypts plaintext, bound to aad.
func (s *Sealer) SealField(aad, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// OpenField decrypts a value that SealField produced for the same aad.
func (s *Sealer) OpenField(aad, sealed []byte) ([]byte, error) {
	if len(sealed) < s.aead.NonceSize() {
		return nil, errors.New("not a sealed value")
	}
	return s.aead.Open(nil, sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():], aad)
}
