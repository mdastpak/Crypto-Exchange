package adminauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/keys"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDataKeys loads the data keys of conn, as an instance does, and returns
// them with their store, to rotate them.
func newDataKeys(t *testing.T, conn *sql.DB) (*keys.Live, *keys.Store) {
	t.Helper()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	store := &keys.Store{DB: conn, Provider: provider}
	live, err := keys.NewLive(context.Background(), store, keys.LiveOptions{})
	require.NoError(t, err)
	return live, store
}

func storedSecret(t *testing.T, conn *sql.DB, adminID string) string {
	t.Helper()
	var stored string
	require.NoError(t, conn.QueryRow(`SELECT totp_secret FROM users WHERE hk = $1`, adminID).Scan(&stored))
	return stored
}

// TestSecrets_AreSealedWithTheDataKeysOnly checks that an authenticator
// secret is stored sealed with a field-enc data key, that it survives a
// rotation, and that nothing else opens: a secret in any other format, or
// one copied from another administrator, is a configuration error and
// never a wrong code.
func TestSecrets_AreSealedWithTheDataKeysOnly(t *testing.T) {
	conn := migratedTestDB(t)
	live, keyStore := newDataKeys(t, conn)
	ctx := context.Background()
	now := time.Unix(1_900_000_000, 0)
	service, err := NewService(conn, live, "")
	require.NoError(t, err)

	first, err := service.Create(ctx, "first@example.com")
	require.NoError(t, err)
	stored := storedSecret(t, conn, first.AdminID)
	require.True(t, strings.HasPrefix(stored, sealedPrefix))
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, sealedPrefix))
	require.NoError(t, err)
	opened, err := live.OpenField(sealingAAD(first.AdminID), sealed)
	require.NoError(t, err, "the field-enc data key opens what is stored")
	assert.Equal(t, decodeSecret(t, first.Secret), opened)
	assert.NotContains(t, stored, first.Secret)
	require.NoError(t, service.VerifyTOTP(ctx, first.AdminID, at(t, service, now, first)))

	// After a rotation, the secret sealed with the older version still
	// opens, and a new one is sealed with the new version.
	_, err = keyStore.Rotate(ctx, keys.PurposeFieldEnc)
	require.NoError(t, err)
	_, err = live.Reload(ctx)
	require.NoError(t, err)
	require.NoError(t, service.VerifyTOTP(ctx, first.AdminID, at(t, service, now.Add(time.Minute), first)))
	second, err := service.Create(ctx, "second@example.com")
	require.NoError(t, err)
	require.NoError(t, service.VerifyTOTP(ctx, second.AdminID, at(t, service, now, second)))

	// A secret copied to another administrator does not open.
	_, err = conn.Exec(`UPDATE users SET totp_secret = (SELECT totp_secret FROM users WHERE hk = $1) WHERE hk = $2`,
		first.AdminID, second.AdminID)
	require.NoError(t, err)
	err = service.VerifyTOTP(ctx, second.AdminID, at(t, service, now.Add(2*time.Minute), first))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCode)

	// No other format is read: not one with another marker, and not a
	// secret in the clear.
	for _, other := range []string{"v9:" + strings.TrimPrefix(stored, sealedPrefix), first.Secret, sealedPrefix + "not base64!"} {
		_, err = conn.Exec(`UPDATE users SET totp_secret = $2 WHERE hk = $1`, first.AdminID, other)
		require.NoError(t, err)
		err = service.VerifyTOTP(ctx, first.AdminID, at(t, service, now.Add(3*time.Minute), first))
		require.Error(t, err, other)
		assert.NotErrorIs(t, err, ErrInvalidCode, other)
	}
}
