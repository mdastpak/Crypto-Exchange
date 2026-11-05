package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomKEK(t testing.TB) string {
	t.Helper()
	kek := make([]byte, KeySize)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(kek)
}

func localProvider(t testing.TB) *LocalProvider {
	t.Helper()
	p, err := NewLocalProvider(randomKEK(t))
	require.NoError(t, err)
	return p
}

func testKey(purpose Purpose, version int) Key {
	return Key{purpose: purpose, version: version, secret: bytes.Repeat([]byte{0xA5}, KeySize)}
}

func TestPurpose_Valid(t *testing.T) {
	for _, p := range Purposes {
		assert.True(t, p.Valid(), p)
	}
	for _, p := range []Purpose{"", "row_mac", "ROW-MAC", "kek"} {
		assert.False(t, p.Valid(), p)
	}
}

func TestKey_NeverPrintsTheSecret(t *testing.T) {
	key := testKey(PurposeFieldEnc, 3)
	secretHex := fmt.Sprintf("%x", key.secret)
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, key)
		assert.NotContains(t, out, secretHex, format)
		assert.NotContains(t, out, "165", format, "the bytes must not appear in decimal either")
		assert.Contains(t, out, "field-enc/v3", format)
	}
}

func TestKey_OffersOnlyTheOperationOfItsPurpose(t *testing.T) {
	for _, purpose := range []Purpose{PurposeRowMAC, PurposeBlindIndex} {
		key := testKey(purpose, 1)
		mac, err := key.NewMAC()
		require.NoError(t, err, purpose)
		mac.Write([]byte("value"))
		assert.Len(t, mac.Sum(nil), 32, "HMAC-SHA-256")
		_, err = key.NewAEAD()
		assert.ErrorIs(t, err, ErrWrongPurpose, "%s must not encrypt", purpose)
	}

	key := testKey(PurposeFieldEnc, 1)
	aead, err := key.NewAEAD()
	require.NoError(t, err)
	assert.Equal(t, 12, aead.NonceSize(), "AES-256-GCM")
	_, err = key.NewMAC()
	assert.ErrorIs(t, err, ErrWrongPurpose, "an encryption key must not seal")

	_, err = Key{}.NewMAC()
	assert.ErrorIs(t, err, ErrWrongPurpose, "the zero key is unusable")
	_, err = Key{}.NewAEAD()
	assert.ErrorIs(t, err, ErrWrongPurpose)
}

func TestBind_TiesTheKeyToItsPurposeAndVersion(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, KeySize)
	bound := bind(PurposeRowMAC, 2, secret)
	assert.NotEqual(t, secret, bound)

	got, err := unbind(bound, PurposeRowMAC, 2)
	require.NoError(t, err)
	assert.Equal(t, secret, got)

	for name, check := range map[string]func() ([]byte, error){
		"another purpose":  func() ([]byte, error) { return unbind(bound, PurposeFieldEnc, 2) },
		"another version":  func() ([]byte, error) { return unbind(bound, PurposeRowMAC, 3) },
		"a truncated key":  func() ([]byte, error) { return unbind(bound[:len(bound)-1], PurposeRowMAC, 2) },
		"a key too long":   func() ([]byte, error) { return unbind(append(bound, 0), PurposeRowMAC, 2) },
		"a raw secret":     func() ([]byte, error) { return unbind(secret, PurposeRowMAC, 2) },
		"version 2 vs 512": func() ([]byte, error) { return unbind(bind(PurposeRowMAC, 512, secret), PurposeRowMAC, 2) },
	} {
		_, err := check()
		assert.ErrorIs(t, err, ErrBinding, name)
	}
}

func TestLocalProvider_WrapsAndUnwraps(t *testing.T) {
	ctx := context.Background()
	p := localProvider(t)
	plaintext := []byte("a data key")

	wrapped, err := p.Wrap(ctx, plaintext)
	require.NoError(t, err)
	assert.NotContains(t, string(wrapped), "a data key")
	again, err := p.Wrap(ctx, plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, wrapped, again, "each wrap uses a new nonce")

	got, err := p.Unwrap(ctx, wrapped)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)

	tampered := append([]byte(nil), wrapped...)
	tampered[len(tampered)-1] ^= 1
	_, err = p.Unwrap(ctx, tampered)
	assert.Error(t, err, "a changed wrapped key must not open")
	_, err = p.Unwrap(ctx, wrapped[:10])
	assert.Error(t, err)

	_, err = localProvider(t).Unwrap(ctx, wrapped)
	assert.Error(t, err, "another KEK must not open the key")
}

func TestLocalProvider_ID(t *testing.T) {
	kek := randomKEK(t)
	a, err := NewLocalProvider(kek)
	require.NoError(t, err)
	b, err := NewLocalProvider(" " + kek + "\n")
	require.NoError(t, err)

	assert.Equal(t, a.ID(), b.ID(), "the same KEK has the same ID")
	assert.NotEqual(t, a.ID(), localProvider(t).ID())
	assert.True(t, strings.HasPrefix(a.ID(), "local:"))
	assert.NotContains(t, a.ID(), strings.TrimRight(kek, "="), "the ID must not reveal the KEK")
	assert.True(t, a.Development())
}

func TestNewLocalProvider_RejectsInvalidKEKs(t *testing.T) {
	for _, kek := range []string{
		"",
		"not base64!",
		base64.StdEncoding.EncodeToString(make([]byte, 16)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
	} {
		_, err := NewLocalProvider(kek)
		assert.ErrorContains(t, err, "KEYS_LOCAL_KEK", "%q", kek)
	}
}

func TestCheckRelease(t *testing.T) {
	local := localProvider(t)
	assert.NoError(t, CheckRelease(local, false))
	assert.ErrorIs(t, CheckRelease(local, true), ErrDevelopmentProvider)

	plain, err := NewTransitProvider(TransitConfig{Address: "http://vault:8200", Token: "t", Key: "k"})
	require.NoError(t, err)
	assert.NoError(t, CheckRelease(plain, false))
	assert.ErrorIs(t, CheckRelease(plain, true), ErrPlainHTTP)

	secure, err := NewTransitProvider(TransitConfig{Address: "https://vault:8200", Token: "t", Key: "k"})
	require.NoError(t, err)
	assert.NoError(t, CheckRelease(secure, true))
}
