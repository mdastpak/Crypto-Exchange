package adminauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
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

func TestSecrets_TheWriteFormatFollowsTheKeys(t *testing.T) {
	conn := migratedTestDB(t)
	live, _ := newDataKeys(t, conn)
	legacy := newKey(t)
	ctx := context.Background()

	i := 0
	for name, tc := range map[string]struct {
		secrets Secrets
		prefix  string
	}{
		"admin.totp_key only":            {Secrets{LegacyKey: legacy}, "v1:"},
		"data keys only":                 {Secrets{DataKeys: live}, "v2:"},
		"both, legacy_secrets write":     {Secrets{LegacyKey: legacy, DataKeys: live, LegacyWrites: true}, "v1:"},
		"both, legacy_secrets read":      {Secrets{LegacyKey: legacy, DataKeys: live}, "v2:"},
		"data keys, write without a key": {Secrets{DataKeys: live, LegacyWrites: true}, "v2:"},
	} {
		t.Run(name, func(t *testing.T) {
			service, err := NewService(conn, tc.secrets, "Crypto Exchange")
			require.NoError(t, err)
			i++
			enrollment, err := service.Create(ctx, fmt.Sprintf("admin%d@example.com", i))
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(storedSecret(t, conn, enrollment.AdminID), tc.prefix))
			now := time.Unix(1_900_000_000, 0)
			assert.NoError(t, service.VerifyTOTP(ctx, enrollment.AdminID, at(t, service, now, enrollment)))
		})
	}
}

// TestSecrets_InstancesReadBothFormats follows the move off admin.totp_key:
// an instance that knows both keys reads secrets of both formats, and one
// that knows only one key names what is missing instead of failing as a
// wrong code.
func TestSecrets_InstancesReadBothFormats(t *testing.T) {
	conn := migratedTestDB(t)
	live, _ := newDataKeys(t, conn)
	legacy := newKey(t)
	ctx := context.Background()
	now := time.Unix(1_900_000_000, 0)

	oldStyle, err := NewService(conn, Secrets{LegacyKey: legacy}, "")
	require.NoError(t, err)
	newStyle, err := NewService(conn, Secrets{DataKeys: live}, "")
	require.NoError(t, err)
	both, err := NewService(conn, Secrets{LegacyKey: legacy, DataKeys: live}, "")
	require.NoError(t, err)

	sealedLegacy, err := oldStyle.Create(ctx, "legacy@example.com")
	require.NoError(t, err)
	sealedData, err := newStyle.Create(ctx, "data@example.com")
	require.NoError(t, err)

	require.NoError(t, both.VerifyTOTP(ctx, sealedLegacy.AdminID, at(t, both, now, sealedLegacy)))
	require.NoError(t, both.VerifyTOTP(ctx, sealedData.AdminID, at(t, both, now, sealedData)))

	err = newStyle.VerifyTOTP(ctx, sealedLegacy.AdminID, at(t, newStyle, now.Add(time.Minute), sealedLegacy))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCode)
	assert.Contains(t, err.Error(), "admin.totp_key, which is not set")
	err = oldStyle.VerifyTOTP(ctx, sealedData.AdminID, at(t, oldStyle, now.Add(time.Minute), sealedData))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys.provider is not set")

	// A data-key secret copied to another administrator does not open.
	_, err = conn.Exec(`UPDATE users SET totp_secret = (SELECT totp_secret FROM users WHERE hk = $1) WHERE hk = $2`,
		sealedData.AdminID, sealedLegacy.AdminID)
	require.NoError(t, err)
	err = both.VerifyTOTP(ctx, sealedLegacy.AdminID, at(t, both, now.Add(2*time.Minute), sealedData))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCode)
}

func TestSecrets_ResealMovesEverySecretAndBack(t *testing.T) {
	conn := migratedTestDB(t)
	live, keyStore := newDataKeys(t, conn)
	legacy := newKey(t)
	ctx := context.Background()
	now := time.Unix(1_900_000_000, 0)

	both, err := NewService(conn, Secrets{LegacyKey: legacy, DataKeys: live, LegacyWrites: true}, "")
	require.NoError(t, err)
	var enrollments []Enrollment
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		e, err := both.Create(ctx, email)
		require.NoError(t, err)
		enrollments = append(enrollments, e)
	}
	require.NoError(t, both.Disable(ctx, "c@example.com"))

	counts, err := both.CountSecrets(ctx)
	require.NoError(t, err)
	assert.Equal(t, SecretCounts{Legacy: 3}, counts)

	changed, err := both.Reseal(ctx, FormatDataKeys)
	require.NoError(t, err)
	assert.Equal(t, 3, changed, "disabled administrators are resealed too, so admin.totp_key can go")
	counts, err = CountSecrets(ctx, conn)
	require.NoError(t, err)
	assert.Equal(t, SecretCounts{DataKeys: 3}, counts)
	changed, err = both.Reseal(ctx, FormatDataKeys)
	require.NoError(t, err)
	assert.Zero(t, changed, "resealing again changes nothing")

	// Without admin.totp_key, the same authenticator apps keep working.
	dataOnly, err := NewService(conn, Secrets{DataKeys: live}, "")
	require.NoError(t, err)
	for _, e := range enrollments[:2] {
		require.NoError(t, dataOnly.VerifyTOTP(ctx, e.AdminID, at(t, dataOnly, now, e)))
	}

	// After a rotation, the secrets sealed with the older version still open.
	_, err = keyStore.Rotate(ctx, keys.PurposeFieldEnc)
	require.NoError(t, err)
	_, err = live.Reload(ctx)
	require.NoError(t, err)
	reset, err := dataOnly.ResetTOTP(ctx, "a@example.com")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(storedSecret(t, conn, reset.AdminID), "v2:"))

	var events int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'admin.totp_resealed'
		AND changes->'before'->>'format' = 'legacy' AND changes->'after'->>'format' = 'data-keys'`).Scan(&events))
	assert.Equal(t, 3, events)

	_, err = dataOnly.Reseal(ctx, FormatLegacy)
	assert.Error(t, err, "resealing to admin.totp_key needs it")

	// The way back, before a rollback to a release that knows only
	// admin.totp_key.
	changed, err = both.Reseal(ctx, FormatLegacy)
	require.NoError(t, err)
	assert.Equal(t, 3, changed)
	oldStyle, err := NewService(conn, Secrets{LegacyKey: legacy}, "")
	require.NoError(t, err)
	later := now.Add(time.Minute)
	require.NoError(t, oldStyle.VerifyTOTP(ctx, reset.AdminID, at(t, oldStyle, later, reset)))
	require.NoError(t, oldStyle.VerifyTOTP(ctx, enrollments[1].AdminID, at(t, oldStyle, later, enrollments[1])))

	_, err = both.Reseal(ctx, "plain")
	assert.Error(t, err)
}
