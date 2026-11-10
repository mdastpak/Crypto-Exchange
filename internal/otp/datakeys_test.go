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

	// The data key is what protects it: the session's contact opens with
	// the keys derived from otp-mac/v1, and a store without the data keys
	// cannot read the session.
	key, err := live.Current(keys.PurposeOTPMAC)
	require.NoError(t, err)
	derived, err := store.dataKeys.Load().derive(key)
	require.NoError(t, err)
	encrypted, err := client.HGet(ctx, store.sessionKey(issued.SessionID), fieldContact).Result()
	require.NoError(t, err)
	opened, err := derived.decryptContact(issued.SessionID, encrypted)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", opened)
	_, err = store.process.decryptContact(issued.SessionID, encrypted)
	assert.Error(t, err, "the keys of the process protect nothing once the data keys are used")
	withoutKeys := NewStore(client, Settings{})
	_, err = withoutKeys.Verify(ctx, issued.SessionID, issued.Code)
	assert.Error(t, err)

	contact, err := store.Verify(ctx, issue(t, store, "bob@example.com").SessionID, "000000")
	assert.ErrorIs(t, err, ErrInvalidCode)
	assert.Empty(t, contact)
	again := issue(t, store, "carol@example.com")
	contact, err = store.Verify(ctx, again.SessionID, again.Code)
	require.NoError(t, err)
	assert.Equal(t, "carol@example.com", contact)
}

// TestStore_InstancesShareSessionsThroughTheDataKeys checks that every
// instance with the data keys verifies the codes of every other, and that a
// session written without a data key is refused by them: nothing but the
// data keys protects a session.
func TestStore_InstancesShareSessionsThroughTheDataKeys(t *testing.T) {
	live, _ := newDataKeys(t)
	client := newTestRedis(t)
	first, second := NewStore(client, Settings{}), NewStore(client, Settings{})
	first.UseDataKeys(live)
	second.UseDataKeys(live)
	ctx := context.Background()

	issued, err := first.Issue(ctx, "dave@example.com", "")
	require.NoError(t, err)
	contact, err := second.Verify(ctx, issued.SessionID, issued.Code)
	require.NoError(t, err)
	assert.Equal(t, "dave@example.com", contact)

	// A store without data keys, as in development, writes no key version.
	withoutKeys := NewStore(client, Settings{})
	issued, err = withoutKeys.Issue(ctx, "erin@example.com", "")
	require.NoError(t, err)
	_, err = first.Verify(ctx, issued.SessionID, issued.Code)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCode)
	assert.Contains(t, err.Error(), "data keys only")
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

	withoutKeys := NewStore(client, Settings{})
	issued := issue(t, store, "heidi@example.com")
	_, err := withoutKeys.Verify(ctx, issued.SessionID, issued.Code)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no data keys"), err.Error())
}
