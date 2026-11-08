package otp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dataKeysTestDBName is the database of the data-key tests of this package.
const dataKeysTestDBName = "testdb_otp_data_keys"

// newDataKeys returns the data keys of a fresh database, as an instance
// loads them, and their store, to rotate them.
func newDataKeys(t *testing.T) (*keys.Live, *keys.Store) {
	t.Helper()
	conn := pgtest.Migrated(t, dataKeysTestDBName)
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	store := &keys.Store{DB: conn, Provider: provider}
	live, err := keys.NewLive(context.Background(), store, keys.LiveOptions{})
	require.NoError(t, err)
	return live, store
}

func TestStore_DataKeysProtectNewSessions(t *testing.T) {
	live, _ := newDataKeys(t)
	store, client := newTestStore(t, Settings{})
	store.UseDataKeys(live)
	ctx := context.Background()

	issued := issue(t, store, "alice@example.com")
	version, err := client.HGet(ctx, store.sessionKey(issued.SessionID), fieldKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "1", version, "the session names the otp-mac version")

	// otp.auth.secret alone cannot read it: the data key is what protects it.
	legacyOnly := NewStore(client, Settings{Secret: testSecret})
	_, err = legacyOnly.Verify(ctx, issued.SessionID, issued.Code)
	assert.Error(t, err)

	contact, err := store.Verify(ctx, issue(t, store, "bob@example.com").SessionID, "000000")
	assert.ErrorIs(t, err, ErrInvalidCode)
	assert.Empty(t, contact)
	again := issue(t, store, "carol@example.com")
	contact, err = store.Verify(ctx, again.SessionID, again.Code)
	require.NoError(t, err)
	assert.Equal(t, "carol@example.com", contact)
}

// TestStore_InstancesOfBothKindsShareSessions follows a rolling change: some
// instances still seal with otp.auth.secret, others with the data keys, and
// every one of them verifies the codes of every other.
func TestStore_InstancesOfBothKindsShareSessions(t *testing.T) {
	live, _ := newDataKeys(t)
	client := newTestRedis(t)
	// A release before #146: the secret only.
	old := NewStore(client, Settings{Secret: testSecret})
	// This release with keys.legacy_secrets "write": it reads the data keys
	// but still writes with the secret.
	writesLegacy := NewStore(client, Settings{Secret: testSecret, LegacyWrites: true})
	writesLegacy.UseDataKeys(live)
	// This release with keys.legacy_secrets "read".
	readsLegacy := NewStore(client, Settings{Secret: testSecret})
	readsLegacy.UseDataKeys(live)
	// The end state: no secret at all.
	dataOnly := NewStore(client, Settings{})
	dataOnly.UseDataKeys(live)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		issuer    *Store
		verifiers []*Store
	}{
		"before #146":             {old, []*Store{writesLegacy, readsLegacy}},
		"legacy_secrets write":    {writesLegacy, []*Store{old, readsLegacy}},
		"legacy_secrets read":     {readsLegacy, []*Store{writesLegacy, dataOnly}},
		"without otp.auth.secret": {dataOnly, []*Store{writesLegacy, readsLegacy}},
	} {
		t.Run(name, func(t *testing.T) {
			for i, verifier := range tc.verifiers {
				issued, err := tc.issuer.Issue(ctx, "dave@example.com", name)
				require.NoError(t, err)
				contact, err := verifier.Verify(ctx, issued.SessionID, issued.Code)
				require.NoError(t, err, "verifier %d", i)
				assert.Equal(t, "dave@example.com", contact)
			}
		})
	}

	issued, err := readsLegacy.Issue(ctx, "erin@example.com", "")
	require.NoError(t, err)
	_, err = old.Verify(ctx, issued.SessionID, issued.Code)
	assert.Error(t, err, "a release before #146 cannot read the data keys, which is why legacy_secrets starts as write")
}

func TestStore_SessionsSurviveARotation(t *testing.T) {
	live, keyStore := newDataKeys(t)
	store, client := newTestStore(t, Settings{})
	store.UseDataKeys(live)
	ctx := context.Background()

	before := issue(t, store, "frank@example.com")
	// The new version is active at once, so the next session uses it.
	version, err := keyStore.Rotate(ctx, keys.PurposeOTPMAC)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	_, err = live.Reload(ctx)
	require.NoError(t, err)

	after := issue(t, store, "frank@example.com")
	stored, err := client.HGet(ctx, store.sessionKey(after.SessionID), fieldKey).Result()
	require.NoError(t, err)
	assert.Equal(t, "2", stored)

	for _, issued := range []Issued{before, after} {
		contact, err := store.Verify(ctx, issued.SessionID, issued.Code)
		require.NoError(t, err, "a session of either version verifies")
		assert.Equal(t, "frank@example.com", contact)
	}
}

func TestStore_UnknownKeyVersionIsAnError(t *testing.T) {
	live, _ := newDataKeys(t)
	store, client := newTestStore(t, Settings{})
	store.UseDataKeys(live)
	ctx := context.Background()

	for _, version := range []string{"9", "x", "0"} {
		issued := issue(t, store, "grace@example.com")
		require.NoError(t, client.HSet(ctx, store.sessionKey(issued.SessionID), fieldKey, version).Err())
		_, err := store.Verify(ctx, issued.SessionID, issued.Code)
		require.Error(t, err, version)
		assert.NotErrorIs(t, err, ErrInvalidCode, "%s: a session that cannot be read is a server error, not a wrong code", version)
	}

	withoutKeys := NewStore(client, Settings{Secret: testSecret})
	issued := issue(t, store, "heidi@example.com")
	_, err := withoutKeys.Verify(ctx, issued.SessionID, issued.Code)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no data keys"), err.Error())
}
