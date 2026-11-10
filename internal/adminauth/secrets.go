package adminauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// FieldSealer seals values with the field-enc data keys. *keys.Live
// implements it.
type FieldSealer interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
}

// ErrNoKeys reports a service without the data keys.
var ErrNoKeys = errors.New("authenticator secrets need the data keys: set keys.provider")

// sealedPrefix marks the format of a stored authenticator secret: base64 of
// what FieldSealer.SealField returns, which holds the key version, the
// nonce, and the ciphertext.
const sealedPrefix = "v1:"

// sealingAAD names the place of a sealed secret: the column and the
// administrator, so that it cannot be copied to another user row.
func sealingAAD(adminID string) []byte { return []byte("users.totp_secret:" + adminID) }

// seal seals secret for adminID with the current field-enc data key.
func (s *Service) seal(adminID string, secret []byte) (string, error) {
	sealed, err := s.dataKeys.SealField(sealingAAD(adminID), secret)
	if err != nil {
		return "", fmt.Errorf("seal the authenticator secret: %w", err)
	}
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// open opens the stored secret of adminID.
func (s *Service) open(adminID, stored string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(stored, sealedPrefix)
	if !ok {
		return nil, errors.New("the stored authenticator secret has an unknown format")
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("the stored authenticator secret is malformed")
	}
	secret, err := s.dataKeys.OpenField(sealingAAD(adminID), sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt the authenticator secret: %w", err)
	}
	return secret, nil
}
