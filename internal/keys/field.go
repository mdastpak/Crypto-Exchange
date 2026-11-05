package keys

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
)

// ErrSealedField reports a sealed value that does not decrypt: it was
// changed, moved to another place, or is not a sealed value at all.
var ErrSealedField = errors.New("the sealed value does not decrypt")

// SealField encrypts plaintext with the current field-enc key. The result
// is the key version in decimal, a colon, a random 96-bit nonce, and the
// AES-256-GCM ciphertext with its tag, so that OpenField finds the key after
// a rotation. aad names the value's place, such as the table, the column,
// and the row; a sealed value copied to another place does not decrypt.
func (r *Keyring) SealField(aad, plaintext []byte) ([]byte, error) {
	key, err := r.Current(PurposeFieldEnc)
	if err != nil {
		return nil, err
	}
	aead, err := key.NewAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := append([]byte(strconv.Itoa(key.version)+":"), nonce...)
	return aead.Seal(sealed, nonce, plaintext, aad), nil
}

// OpenField decrypts a value that SealField produced for the same aad, with
// whichever loaded version of the field-enc key sealed it.
func (r *Keyring) OpenField(aad, sealed []byte) ([]byte, error) {
	colon := bytes.IndexByte(sealed, ':')
	if colon < 1 {
		return nil, ErrSealedField
	}
	version, err := strconv.Atoi(string(sealed[:colon]))
	if err != nil || version < 1 || strconv.Itoa(version) != string(sealed[:colon]) {
		return nil, ErrSealedField
	}
	key, err := r.Version(PurposeFieldEnc, version)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSealedField, err)
	}
	aead, err := key.NewAEAD()
	if err != nil {
		return nil, err
	}
	rest := sealed[colon+1:]
	if len(rest) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrSealedField
	}
	plaintext, err := aead.Open(nil, rest[:aead.NonceSize()], rest[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrSealedField
	}
	return plaintext, nil
}
