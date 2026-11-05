package keys

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/account"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRing returns a keyring with the given versions of the field-enc key.
func testRing(t *testing.T, versions ...int) *Keyring {
	t.Helper()
	ring := &Keyring{keys: make(map[Purpose][]Key)}
	for _, v := range versions {
		secret := make([]byte, KeySize)
		_, err := rand.Read(secret)
		require.NoError(t, err)
		ring.keys[PurposeFieldEnc] = append(ring.keys[PurposeFieldEnc], Key{purpose: PurposeFieldEnc, version: v, secret: secret})
	}
	return ring
}

func TestSealField_RoundTrip(t *testing.T) {
	ring := testRing(t, 1, 2)
	aad := []byte("api_keys.secret_sealed:hmac_0123")

	sealed, err := ring.SealField(aad, []byte("the secret"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(sealed, []byte("2:")), "the current version seals")
	assert.NotContains(t, string(sealed), "the secret")
	again, err := ring.SealField(aad, []byte("the secret"))
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again, "each seal uses a new nonce")

	plaintext, err := ring.OpenField(aad, sealed)
	require.NoError(t, err)
	assert.Equal(t, "the secret", string(plaintext))
}

func TestOpenField_ReadsOlderVersions(t *testing.T) {
	old := testRing(t, 1)
	sealed, err := old.SealField([]byte("place"), []byte("value"))
	require.NoError(t, err)

	rotated := &Keyring{keys: map[Purpose][]Key{PurposeFieldEnc: append(old.keys[PurposeFieldEnc], testRing(t, 2).keys[PurposeFieldEnc]...)}}
	plaintext, err := rotated.OpenField([]byte("place"), sealed)
	require.NoError(t, err)
	assert.Equal(t, "value", string(plaintext))

	retired := testRing(t, 2)
	_, err = retired.OpenField([]byte("place"), sealed)
	assert.ErrorIs(t, err, ErrSealedField)
	assert.ErrorIs(t, err, ErrNoKey, "the message says that version 1 is gone")
}

func TestOpenField_RefusesChangedOrMovedValues(t *testing.T) {
	ring := testRing(t, 1)
	sealed, err := ring.SealField([]byte("api_keys.secret_sealed:a"), []byte("secret"))
	require.NoError(t, err)

	flipped := append([]byte(nil), sealed...)
	flipped[len(flipped)-1] ^= 1
	for name, tc := range map[string]struct {
		aad    string
		sealed []byte
	}{
		"moved to another row":  {"api_keys.secret_sealed:b", sealed},
		"changed ciphertext":    {"api_keys.secret_sealed:a", flipped},
		"truncated":             {"api_keys.secret_sealed:a", sealed[:10]},
		"no version":            {"api_keys.secret_sealed:a", sealed[2:]},
		"version with a zero":   {"api_keys.secret_sealed:a", append([]byte("01"), sealed[1:]...)},
		"another version":       {"api_keys.secret_sealed:a", append([]byte("7"), sealed[1:]...)},
		"not a sealed value":    {"api_keys.secret_sealed:a", []byte("plaintext")},
		"empty":                 {"api_keys.secret_sealed:a", nil},
		"version zero":          {"api_keys.secret_sealed:a", append([]byte("0"), sealed[1:]...)},
		"negative version":      {"api_keys.secret_sealed:a", append([]byte("-1"), sealed[1:]...)},
		"only a version prefix": {"api_keys.secret_sealed:a", []byte("1:")},
	} {
		_, err := ring.OpenField([]byte(tc.aad), tc.sealed)
		assert.ErrorIs(t, err, ErrSealedField, name)
	}
}

func TestSealField_NeedsAFieldKey(t *testing.T) {
	_, err := (&Keyring{keys: map[Purpose][]Key{}}).SealField(nil, []byte("x"))
	assert.ErrorIs(t, err, ErrNoKey)
}

// The API keys of the account package seal their secrets with a keyring.
var _ account.FieldSealer = (*Keyring)(nil)

func TestSealAPIKeySecret_WithAKeyring(t *testing.T) {
	ring := testRing(t, 1)
	key, err := account.GenerateAPIKey(account.APIKeyTypeHMAC)
	require.NoError(t, err)

	sealed, err := account.SealAPIKeySecret(ring, key.KeyID, []byte(key.Secret))
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), key.Secret)
	opened, err := account.OpenAPIKeySecret(ring, key.KeyID, sealed)
	require.NoError(t, err)
	assert.Equal(t, key.Secret, string(opened))

	other, err := account.NewAPIKeyID(account.APIKeyTypeHMAC)
	require.NoError(t, err)
	_, err = account.OpenAPIKeySecret(ring, other, sealed)
	assert.ErrorIs(t, err, ErrSealedField, "a secret copied to another key's row does not open")
}
