package field_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_field"

// testKeys returns the store of the data keys of a new local KEK, and the
// keyring it loads.
func testKeys(t *testing.T) (*keys.Store, *keys.Keyring) {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	store := &keys.Store{DB: pgtest.Migrated(t, testDBName), Provider: provider}
	ring, err := store.Load(context.Background())
	require.NoError(t, err)
	return store, ring
}

// rotate creates the next version of purpose, active at once, and returns
// the keyring that loads it.
func rotate(t *testing.T, store *keys.Store, purpose keys.Purpose) *keys.Keyring {
	t.Helper()
	_, err := store.Rotate(context.Background(), purpose)
	require.NoError(t, err)
	ring, err := store.Load(context.Background())
	require.NoError(t, err)
	return ring
}

var place = field.Place{Table: "user_contacts", Column: "value", Row: "42"}

func TestCipher_SealsAndOpens(t *testing.T) {
	_, ring := testKeys(t)
	c := field.New(ring)

	sealed, err := c.Seal(place, "someone@example.com")
	require.NoError(t, err)
	assert.False(t, bytes.Contains(sealed, []byte("someone")), "the sealed value holds no plaintext")
	version, ok := field.Version(sealed)
	require.True(t, ok)
	assert.Equal(t, 1, version)

	again, err := c.Seal(place, "someone@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again, "each seal has its own nonce")

	opened, err := c.Open(place, sealed)
	require.NoError(t, err)
	assert.Equal(t, "someone@example.com", opened)
}

func TestCipher_RefusesAValueMovedOrChanged(t *testing.T) {
	_, ring := testKeys(t)
	c := field.New(ring)
	sealed, err := c.Seal(place, "someone@example.com")
	require.NoError(t, err)

	for name, other := range map[string]field.Place{
		"another row":    {Table: "user_contacts", Column: "value", Row: "43"},
		"another column": {Table: "user_contacts", Column: "ownership_ref", Row: "42"},
		"another table":  {Table: "users", Column: "value", Row: "42"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Open(other, sealed)
			require.ErrorIs(t, err, keys.ErrSealedField)
		})
	}

	t.Run("a changed ciphertext", func(t *testing.T) {
		for i := len(field.Prefix(1)); i < len(sealed); i++ {
			changed := bytes.Clone(sealed)
			changed[i] ^= 0x01
			_, err := c.Open(place, changed)
			require.ErrorIs(t, err, keys.ErrSealedField, "byte %d", i)
		}
	})
	t.Run("a cut ciphertext", func(t *testing.T) {
		_, err := c.Open(place, sealed[:len(sealed)-1])
		require.ErrorIs(t, err, keys.ErrSealedField)
	})
	t.Run("another version", func(t *testing.T) {
		changed := append(field.Prefix(2), sealed[len(field.Prefix(1)):]...)
		_, err := c.Open(place, changed)
		require.ErrorIs(t, err, keys.ErrSealedField)
	})
}

func TestCipher_ErrorsHoldNoPlaintext(t *testing.T) {
	_, ring := testKeys(t)
	c := field.New(ring)
	sealed, err := c.Seal(place, "someone@example.com")
	require.NoError(t, err)
	_, err = c.Open(field.Place{Table: "user_contacts", Column: "value", Row: "1"}, sealed)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "someone")
	assert.NotContains(t, err.Error(), "example")
}

func TestCipher_OpensValuesOfAnOlderVersion(t *testing.T) {
	store, ring := testKeys(t)
	sealed, err := field.New(ring).Seal(place, "someone@example.com")
	require.NoError(t, err)

	c := field.New(rotate(t, store, keys.PurposeFieldEnc))
	sealing, _, err := c.CurrentVersions()
	require.NoError(t, err)
	assert.Equal(t, 2, sealing)
	opened, err := c.Open(place, sealed)
	require.NoError(t, err)
	assert.Equal(t, "someone@example.com", opened)

	newer, err := c.Seal(place, "someone@example.com")
	require.NoError(t, err)
	version, _ := field.Version(newer)
	assert.Equal(t, 2, version)
}

func TestCipher_BlindIndexes(t *testing.T) {
	store, ring := testKeys(t)
	c := field.New(ring)

	idx, err := c.Index("user_contacts.value:email", "someone@example.com")
	require.NoError(t, err)
	again, err := c.Index("user_contacts.value:email", "someone@example.com")
	require.NoError(t, err)
	assert.Equal(t, idx, again, "an index is the same for the same value")
	assert.False(t, bytes.Contains(idx, []byte("someone")))
	version, ok := field.Version(idx)
	require.True(t, ok)
	assert.Equal(t, 1, version)

	other, err := c.Index("user_contacts.value:email", "someone@example.org")
	require.NoError(t, err)
	assert.NotEqual(t, idx, other)
	otherDomain, err := c.Index("user_contacts.value:username", "someone@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, idx, otherDomain, "a domain has indexes of its own")

	assert.Equal(t, [][]byte{idx}, c.Indexes("user_contacts.value:email", "someone@example.com"))

	t.Run("after a rotation", func(t *testing.T) {
		rotated := field.New(rotate(t, store, keys.PurposeBlindIndex))
		newer, err := rotated.Index("user_contacts.value:email", "someone@example.com")
		require.NoError(t, err)
		assert.NotEqual(t, idx, newer)
		version, _ := field.Version(newer)
		assert.Equal(t, 2, version)
		assert.Equal(t, [][]byte{newer, idx}, rotated.Indexes("user_contacts.value:email", "someone@example.com"),
			"a lookup also finds the rows indexed with the older version")
	})
}

func TestCipher_Nil(t *testing.T) {
	var c *field.Cipher
	_, err := c.Seal(place, "x")
	require.ErrorIs(t, err, field.ErrNoCipher)
	_, err = c.Open(place, []byte("1:x"))
	require.ErrorIs(t, err, field.ErrNoCipher)
	_, err = c.Index("d", "x")
	require.ErrorIs(t, err, field.ErrNoCipher)
	assert.Empty(t, c.Indexes("d", "x"))
	assert.NotNil(t, c.Indexes("d", "x"), "an empty array, which a query compares with no row")
}

func TestVersion(t *testing.T) {
	for in, want := range map[string]int{"1:abc": 1, "12:": 12} {
		v, ok := field.Version([]byte(in))
		assert.True(t, ok, in)
		assert.Equal(t, want, v, in)
	}
	for _, in := range []string{"", ":abc", "0:abc", "01:abc", "-1:abc", "x:abc", "abc"} {
		_, ok := field.Version([]byte(in))
		assert.False(t, ok, in)
	}
}

func TestUse(t *testing.T) {
	_, ring := testKeys(t)
	c := field.New(ring)
	restore := field.Use(c)
	assert.Same(t, c, field.Active())
	restore()
	assert.Nil(t, field.Active())
}
