package keys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Provider wraps and unwraps data keys with a key-encryption key (KEK) that
// it never reveals.
type Provider interface {
	// ID identifies the KEK. It is stored with every wrapped key, so that a
	// key wrapped by another KEK is reported as such.
	ID() string
	// Development reports a provider meant only for development and tests;
	// release mode refuses it.
	Development() bool
	Wrap(ctx context.Context, plaintext []byte) ([]byte, error)
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
}

var (
	// ErrDevelopmentProvider is returned for a development provider in
	// release mode.
	ErrDevelopmentProvider = errors.New("keys.provider \"local\" keeps the key-encryption key in the configuration and is refused in release mode; use \"transit\"")
	// ErrPlainHTTP is returned for a Transit address without TLS in release
	// mode.
	ErrPlainHTTP = errors.New("keys.transit.address must use https in release mode: over http, the token and the data keys travel unencrypted")
)

// CheckRelease refuses, in release mode, a development provider and a
// provider that talks to its key management service without TLS.
func CheckRelease(p Provider, releaseMode bool) error {
	if !releaseMode {
		return nil
	}
	if p.Development() {
		return ErrDevelopmentProvider
	}
	if plain, ok := p.(interface{ PlainHTTP() bool }); ok && plain.PlainHTTP() {
		return ErrPlainHTTP
	}
	return nil
}

// LocalProvider wraps keys with AES-256-GCM under a KEK from the
// configuration. Whoever reads the configuration can unwrap every data key,
// so it is for development and tests only.
type LocalProvider struct {
	aead cipher.AEAD
	id   string
}

// NewLocalProvider takes the KEK as base64 of 32 random bytes, for example
// from "openssl rand -base64 32".
func NewLocalProvider(encoded string) (*LocalProvider, error) {
	kek, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(kek) != KeySize {
		return nil, fmt.Errorf("keys.local.kek (KEYS_LOCAL_KEK) must be base64 of %d random bytes", KeySize)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The ID is a fingerprint, which reveals nothing about the KEK.
	sum := sha256.Sum256(append([]byte("crypto-exchange/local-kek-id\x00"), kek...))
	return &LocalProvider{aead: aead, id: "local:" + hex.EncodeToString(sum[:8])}, nil
}

func (p *LocalProvider) ID() string        { return p.id }
func (p *LocalProvider) Development() bool { return true }

// Wrap returns the nonce followed by the sealed plaintext.
func (p *LocalProvider) Wrap(_ context.Context, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return p.aead.Seal(nonce, nonce, plaintext, []byte(p.id)), nil
}

func (p *LocalProvider) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	if len(wrapped) < p.aead.NonceSize()+p.aead.Overhead() {
		return nil, errors.New("the wrapped key is too short")
	}
	nonce, sealed := wrapped[:p.aead.NonceSize()], wrapped[p.aead.NonceSize():]
	plaintext, err := p.aead.Open(nil, nonce, sealed, []byte(p.id))
	if err != nil {
		return nil, errors.New("the wrapped key does not open with this key-encryption key")
	}
	return plaintext, nil
}
