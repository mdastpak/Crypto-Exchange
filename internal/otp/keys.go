package otp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"sync"

	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// DataKeys supplies the otp-mac data keys. *keys.Live implements it:
// Current is the version that writes now, and Version finds an older or a
// newer one, reloading the keys when another instance already uses a
// version that this one has not loaded.
type DataKeys interface {
	Current(purpose keys.Purpose) (keys.Key, error)
	Version(ctx context.Context, purpose keys.Purpose, version int) (keys.Key, error)
}

// The labels of the keys derived from a root secret.
const (
	labelContacts = "otp contact encryption"
	labelMAC      = "otp code and contact hashing"
)

// processVersion is the version of the keys of a store without data keys.
// No data key has it: their versions start at 1.
const processVersion = 0

// sessionKeys are the keys of a sign-in session, derived from one root: one
// version of the otp-mac data key, or the random secret of a store without
// data keys.
type sessionKeys struct {
	version  int
	macKey   []byte
	contacts cipher.AEAD
}

// newSessionKeys derives the keys of version from root, which computes an
// HMAC keyed by the root secret.
func newSessionKeys(version int, root func() hash.Hash) (*sessionKeys, error) {
	derive := func(label string) []byte {
		mac := root()
		mac.Write([]byte(label))
		return mac.Sum(nil)
	}
	block, err := aes.NewCipher(derive(labelContacts))
	if err != nil {
		return nil, err
	}
	contacts, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sessionKeys{version: version, macKey: derive(labelMAC), contacts: contacts}, nil
}

// processSessionKeys derives keys from a random secret that only this
// process knows, for a store without data keys.
func processSessionKeys() *sessionKeys {
	secret := make([]byte, sha256.Size)
	_, _ = rand.Read(secret) // never fails since Go 1.24
	k, err := newSessionKeys(processVersion, func() hash.Hash { return hmac.New(sha256.New, secret) })
	if err != nil {
		panic(err) // unreachable: the derived key is always 32 bytes
	}
	return k
}

func (k *sessionKeys) mac(parts ...string) []byte {
	mac := hmac.New(sha256.New, k.macKey)
	mac.Write([]byte(strings.Join(parts, "\x00")))
	return mac.Sum(nil)
}

// contactID identifies a contact in Redis keys. Email addresses are matched
// case-insensitively, like the users table does.
func (k *sessionKeys) contactID(contact string) string {
	return hex.EncodeToString(k.mac("contact", strings.ToLower(contact)))
}

// clientID identifies a client in Redis keys, without its address.
func (k *sessionKeys) clientID(client string) string {
	return hex.EncodeToString(k.mac("client", client))
}

// storedVersion is the key version as a session stores it: empty for the
// keys of a store without data keys.
func (k *sessionKeys) storedVersion() string {
	if k.version == processVersion {
		return ""
	}
	return strconv.Itoa(k.version)
}

// encryptContact encrypts contact with AES-GCM. The session ID is
// authenticated data, so a ciphertext cannot be moved to another session.
func (k *sessionKeys) encryptContact(sessionID, contact string) (string, error) {
	nonce := make([]byte, k.contacts.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	sealed := k.contacts.Seal(nonce, nonce, []byte(contact), []byte(sessionID))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (k *sessionKeys) decryptContact(sessionID, encrypted string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(sealed) < k.contacts.NonceSize() {
		return "", errors.New("decrypt the contact: malformed ciphertext")
	}
	nonce, ciphertext := sealed[:k.contacts.NonceSize()], sealed[k.contacts.NonceSize():]
	contact, err := k.contacts.Open(nil, nonce, ciphertext, []byte(sessionID))
	if err != nil {
		// The session was written with another key, or changed in Redis.
		return "", fmt.Errorf("decrypt the contact: %w", err)
	}
	return string(contact), nil
}

// keySource holds the data keys of a store and the keys derived from each
// version, which never change for a version.
type keySource struct {
	keys    DataKeys
	mu      sync.Mutex
	derived map[int]*sessionKeys
}

func (s *keySource) derive(key keys.Key) (*sessionKeys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.derived[key.Version()]; ok {
		return k, nil
	}
	if _, err := key.NewMAC(); err != nil {
		return nil, err
	}
	k, err := newSessionKeys(key.Version(), func() hash.Hash {
		mac, _ := key.NewMAC() // checked above
		return mac
	})
	if err != nil {
		return nil, err
	}
	s.derived[key.Version()] = k
	return k, nil
}

// UseDataKeys makes the store protect its sessions with the otp-mac data
// keys, which every instance shares, and with nothing else: a session
// without a key version is then refused. Call it before the store is used.
func (s *Store) UseDataKeys(dataKeys DataKeys) {
	s.dataKeys.Store(&keySource{keys: dataKeys, derived: make(map[int]*sessionKeys)})
}

// writer returns the keys that protect a new session.
func (s *Store) writer() (*sessionKeys, error) {
	src := s.dataKeys.Load()
	if src == nil {
		return s.process, nil
	}
	key, err := src.keys.Current(keys.PurposeOTPMAC)
	if err != nil {
		return nil, fmt.Errorf("the otp-mac data key: %w", err)
	}
	return src.derive(key)
}

// reader returns the keys that wrote a session, from its stored version.
func (s *Store) reader(ctx context.Context, stored string) (*sessionKeys, error) {
	src := s.dataKeys.Load()
	if stored == "" {
		if src != nil {
			return nil, errors.New("the session names no data key, and this instance protects sessions with data keys only")
		}
		return s.process, nil
	}
	version, err := strconv.Atoi(stored)
	if err != nil || version < 1 {
		return nil, fmt.Errorf("the session names the key version %q", stored)
	}
	if src == nil {
		return nil, errors.New("the session was written with a data key, and this instance has no data keys")
	}
	key, err := src.keys.Version(ctx, keys.PurposeOTPMAC, version)
	if err != nil {
		return nil, fmt.Errorf("the otp-mac data key: %w", err)
	}
	return src.derive(key)
}
