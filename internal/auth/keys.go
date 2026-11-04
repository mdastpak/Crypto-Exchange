package auth

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/golang-jwt/jwt/v5"
)

// Roles. Each role has its own signing keys and audience, so a token issued
// for one role is never accepted for another, even if its role claim was
// forged with a stolen key.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// minRSAKeyBits is the smallest accepted RSA modulus.
const minRSAKeyBits = 2048

// audienceFor returns the "aud" claim of tokens for role.
func audienceFor(role string) string {
	return role + "-api"
}

func knownRole(role string) bool {
	return role == RoleUser || role == RoleAdmin
}

// KeyPair holds the PEM-encoded keys of one role.
type KeyPair struct {
	// PrivateKey signs new tokens. PublicKey must belong to it.
	PrivateKey []byte
	PublicKey  []byte
	// Retired holds the public keys of earlier key pairs. Tokens they signed
	// stay valid until they expire, so keys can be rotated without signing
	// everyone out.
	Retired [][]byte
}

// roleKeys are the parsed keys of one role.
type roleKeys struct {
	signing    *rsa.PrivateKey
	signingKID string
	// verifying maps a key ID to a public key, including the signing key's.
	verifying map[string]*rsa.PublicKey
}

// keyring is never modified after it is built; SetKeys replaces it as a
// whole, so readers need no lock.
type keyring struct {
	roles map[string]*roleKeys
}

var currentKeys atomic.Pointer[keyring]

// SetKeys installs the keys of every role at once, replacing earlier keys. A
// role without keys can neither issue nor accept tokens. Keys must be RSA
// keys of at least 2048 bits, and no key may be shared between roles.
func SetKeys(pairs map[string]KeyPair) error {
	ring := &keyring{roles: make(map[string]*roleKeys, len(pairs))}
	owner := make(map[string]string) // key ID -> role, to reject shared keys
	for role, pair := range pairs {
		if !knownRole(role) {
			return fmt.Errorf("%w: %q", ErrUnknownRole, role)
		}
		keys, err := parseKeyPair(pair)
		if err != nil {
			return fmt.Errorf("%s keys: %w", role, err)
		}
		for kid := range keys.verifying {
			if other, taken := owner[kid]; taken {
				return fmt.Errorf("%s and %s must not share a key", other, role)
			}
			owner[kid] = role
		}
		ring.roles[role] = keys
	}
	currentKeys.Store(ring)
	return nil
}

// InitKeys installs the user keys only. Admin tokens can then neither be
// issued nor accepted. The server uses SetKeys.
func InitKeys(privateKeyPEM, publicKeyPEM []byte) error {
	return SetKeys(map[string]KeyPair{RoleUser: {PrivateKey: privateKeyPEM, PublicKey: publicKeyPEM}})
}

func parseKeyPair(pair KeyPair) (*roleKeys, error) {
	private, err := jwt.ParseRSAPrivateKeyFromPEM(pair.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	public, err := jwt.ParseRSAPublicKeyFromPEM(pair.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if !private.PublicKey.Equal(public) {
		return nil, errors.New("the public key does not belong to the private key")
	}
	kid, err := checkedKeyID(public)
	if err != nil {
		return nil, err
	}

	keys := &roleKeys{signing: private, signingKID: kid, verifying: map[string]*rsa.PublicKey{kid: public}}
	for i, pemData := range pair.Retired {
		retired, err := jwt.ParseRSAPublicKeyFromPEM(pemData)
		if err != nil {
			return nil, fmt.Errorf("retired public key %d: %w", i+1, err)
		}
		retiredKID, err := checkedKeyID(retired)
		if err != nil {
			return nil, fmt.Errorf("retired public key %d: %w", i+1, err)
		}
		keys.verifying[retiredKID] = retired
	}
	return keys, nil
}

func checkedKeyID(key *rsa.PublicKey) (string, error) {
	if key.N.BitLen() < minRSAKeyBits {
		return "", fmt.Errorf("RSA keys must have at least %d bits, got %d", minRSAKeyBits, key.N.BitLen())
	}
	return KeyID(key)
}

// KeyID derives the "kid" header of tokens signed with the private key of
// key, from a SHA-256 hash of the public key. Every server instance derives
// the same ID, so key IDs need no configuration.
func KeyID(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:16]), nil
}

// keysFor returns the keys of role, or an error when the role is unknown or
// has no keys.
func keysFor(role string) (*roleKeys, error) {
	if !knownRole(role) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	ring := currentKeys.Load()
	if ring == nil || ring.roles[role] == nil {
		return nil, ErrKeysNotLoaded
	}
	return ring.roles[role], nil
}

// KeysLoaded reports whether keys are installed for role.
func KeysLoaded(role string) bool {
	_, err := keysFor(role)
	return err == nil
}
