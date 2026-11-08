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

// DataKeys supplies the otp-mac data keys (#146). *keys.Live implements it:
// Current is the version that writes now, and Version finds an older or a
// newer one, reloading the keys when another instance already uses a
// version that this one has not loaded.
type DataKeys interface {
	Current(purpose keys.Purpose) (keys.Key, error)
	Version(ctx context.Context, purpose keys.Purpose, version int) (keys.Key, error)
}

// The labels of the keys derived from a root secret. The labels are those
// of the releases before #146, so a store keyed by otp.auth.secret reads the
// sessions that those releases wrote.
const (
	labelContacts = "otp contact encryption"
	labelMAC      = "otp code and contact hashing"
)

// legacyVersion is the version of the keys derived from otp.auth.secret.
// Sessions written with it carry no key version, as before #146.
const legacyVersion = 0

// sessionKeys are the keys of a sign-in session, derived from one root:
// otp.auth.secret, or one version of the otp-mac data key.
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

// legacySessionKeys derives the keys of otp.auth.secret.
func legacySessionKeys(secret []byte) *sessionKeys {
	k, err := newSessionKeys(legacyVersion, func() hash.Hash { return hmac.New(sha256.New, secret) })
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

// storedVersion is the key version as a session stores it: empty for
// otp.auth.secret, as sessions of earlier releases have it.
func (k *sessionKeys) storedVersion() string {
	if k.version == legacyVersion {
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
		// Usually a changed secret: sessions created before the change
		// cannot be read anymore.
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

// UseDataKeys makes the store protect new sessions with the otp-mac data
// keys instead of otp.auth.secret (#146), unless the secret is set and
// Settings.LegacyWrites asks to keep writing with it. Sessions are read with
// whichever keys wrote them, so stores of both kinds share one Redis while
// a release or a change of keys.legacy_secrets rolls out. Call it before
// the store is used.
func (s *Store) UseDataKeys(dataKeys DataKeys) {
	s.dataKeys.Store(&keySource{keys: dataKeys, derived: make(map[int]*sessionKeys)})
}

// writer returns the keys that protect a new session.
func (s *Store) writer() (*sessionKeys, error) {
	src := s.dataKeys.Load()
	if src == nil || (s.legacySet && s.settings.LegacyWrites) {
		return s.legacy, nil
	}
	key, err := src.keys.Current(keys.PurposeOTPMAC)
	if err != nil {
		return nil, fmt.Errorf("the otp-mac data key: %w", err)
	}
	return src.derive(key)
}

// reader returns the keys that wrote a session, from its stored version.
func (s *Store) reader(ctx context.Context, stored string) (*sessionKeys, error) {
	if stored == "" {
		return s.legacy, nil
	}
	version, err := strconv.Atoi(stored)
	if err != nil || version < 1 {
		return nil, fmt.Errorf("the session names the key version %q", stored)
	}
	src := s.dataKeys.Load()
	if src == nil {
		return nil, errors.New("the session was written with a data key, and this instance has no data keys")
	}
	key, err := src.keys.Version(ctx, keys.PurposeOTPMAC, version)
	if err != nil {
		return nil, fmt.Errorf("the otp-mac data key: %w", err)
	}
	return src.derive(key)
}
