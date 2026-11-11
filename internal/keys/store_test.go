package keys

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const storeTestDBName = "testdb_keys"

func newTestStore(t *testing.T, provider Provider) (*Store, *sql.DB) {
	t.Helper()
	conn := pgtest.Migrated(t, storeTestDBName)
	return &Store{DB: conn, Provider: provider}, conn
}

// fingerprint is the MAC of a fixed message, which tells keys apart without
// exposing them.
func fingerprint(t *testing.T, key Key) string {
	t.Helper()
	if key.Purpose() == PurposeFieldEnc {
		aead, err := key.NewAEAD()
		require.NoError(t, err)
		return string(aead.Seal(nil, make([]byte, aead.NonceSize()), []byte("fingerprint"), nil))
	}
	mac, err := key.NewMAC()
	require.NoError(t, err)
	mac.Write([]byte("fingerprint"))
	return string(mac.Sum(nil))
}

func TestStore_LoadCreatesTheFirstKeysOnce(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	store, conn := newTestStore(t, provider)

	ring, err := store.Load(ctx)
	require.NoError(t, err)
	first := make(map[Purpose]string)
	for _, purpose := range Purposes {
		key, err := ring.Current(purpose)
		require.NoError(t, err, purpose)
		assert.Equal(t, purpose, key.Purpose())
		assert.Equal(t, 1, key.Version())
		first[purpose] = fingerprint(t, key)
	}
	assert.NotEqual(t, first[PurposeRowMAC], first[PurposeBlindIndex], "every purpose has a key of its own")

	var rows int
	var kekID string
	var wrapped []byte
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Equal(t, len(Purposes), rows)
	require.NoError(t, conn.QueryRow(`SELECT kek_id, wrapped_key FROM data_keys WHERE purpose = 'row-mac'`).Scan(&kekID, &wrapped))
	assert.Equal(t, provider.ID(), kekID)

	// A restart loads the same keys and creates none.
	ring, err = store.Load(ctx)
	require.NoError(t, err)
	for _, purpose := range Purposes {
		key, err := ring.Current(purpose)
		require.NoError(t, err)
		assert.Equal(t, first[purpose], fingerprint(t, key), "%s must survive a restart", purpose)
	}
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Equal(t, len(Purposes), rows)
}

func TestStore_ConcurrentFirstLoadsCreateOneKeyPerPurpose(t *testing.T) {
	ctx := context.Background()
	store, conn := newTestStore(t, localProvider(t))
	const instances = 6

	rings := make([]*Keyring, instances)
	errs := make([]error, instances)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rings[i], errs[i] = store.Load(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i], "instance %d", i)
	}
	var rows int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Equal(t, len(Purposes), rows)
	for _, purpose := range Purposes {
		want, err := rings[0].Current(purpose)
		require.NoError(t, err)
		for i := 1; i < instances; i++ {
			got, err := rings[i].Current(purpose)
			require.NoError(t, err)
			assert.Equal(t, fingerprint(t, want), fingerprint(t, got), "every instance uses the same %s key", purpose)
		}
	}
}

func TestStore_RotateAndRetire(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, localProvider(t))
	ring, err := store.Load(ctx)
	require.NoError(t, err)
	v1, err := ring.Current(PurposeRowMAC)
	require.NoError(t, err)

	version, err := store.Rotate(ctx, PurposeRowMAC)
	require.NoError(t, err)
	assert.Equal(t, 2, version)
	_, err = store.Rotate(ctx, "kek")
	assert.Error(t, err)

	ring, err = store.Load(ctx)
	require.NoError(t, err)
	current, err := ring.Current(PurposeRowMAC)
	require.NoError(t, err)
	assert.Equal(t, 2, current.Version(), "the new version is current after the next load")
	assert.NotEqual(t, fingerprint(t, v1), fingerprint(t, current))
	old, err := ring.Version(PurposeRowMAC, 1)
	require.NoError(t, err)
	assert.Equal(t, fingerprint(t, v1), fingerprint(t, old), "the old version stays readable")
	other, err := ring.Current(PurposeFieldEnc)
	require.NoError(t, err)
	assert.Equal(t, 1, other.Version(), "other purposes are not rotated")

	err = store.Retire(ctx, PurposeRowMAC, 2)
	assert.ErrorContains(t, err, "newest version")
	err = store.Retire(ctx, PurposeRowMAC, 7)
	assert.ErrorIs(t, err, ErrNoKey)
	require.NoError(t, store.Retire(ctx, PurposeRowMAC, 1))
	err = store.Retire(ctx, PurposeRowMAC, 1)
	assert.ErrorContains(t, err, "already retired")

	ring, err = store.Load(ctx)
	require.NoError(t, err)
	_, err = ring.Version(PurposeRowMAC, 1)
	assert.ErrorIs(t, err, ErrNoKey, "a retired version is no longer loaded")
	current, err = ring.Current(PurposeRowMAC)
	require.NoError(t, err)
	assert.Equal(t, 2, current.Version())

	infos, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, len(Purposes)+1)
	states := make(map[string]bool)
	for _, info := range infos {
		states[fmt.Sprintf("%s/v%d", info.Purpose, info.Version)] = info.Current
		if info.Purpose == PurposeRowMAC && info.Version == 1 {
			assert.NotNil(t, info.RetiredAt)
		}
	}
	assert.Equal(t, map[string]bool{"row-mac/v1": false, "row-mac/v2": true, "field-enc/v1": true, "blind-index/v1": true, "otp-mac/v1": true,
		"audit-mac/v1": true}, states)
}

func TestStore_LoadRefusesKeysItCannotTrust(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)

	for name, tc := range map[string]struct {
		change string
		want   string
	}{
		"wrapped by another KEK": {
			change: `UPDATE data_keys SET kek_id = 'local:0000000000000000' WHERE purpose = 'field-enc'`,
			want:   "was wrapped by the key-encryption key",
		},
		"wrapped key changed": {
			change: `UPDATE data_keys SET wrapped_key = set_byte(wrapped_key, 20, get_byte(wrapped_key, 20) # 1) WHERE purpose = 'field-enc'`,
			want:   "unwrapping data key field-enc/v1",
		},
		"wrapped key copied to another purpose": {
			change: `UPDATE data_keys SET wrapped_key = (SELECT wrapped_key FROM data_keys WHERE purpose = 'row-mac') WHERE purpose = 'field-enc'`,
			want:   "does not belong to its row",
		},
		"newest version retired by SQL": {
			change: `UPDATE data_keys SET retired_at = NOW() WHERE purpose = 'field-enc' AND version = 2`,
			want:   "the newest data key field-enc/v2 is retired",
		},
		"every version retired": {
			change: `UPDATE data_keys SET retired_at = NOW() WHERE purpose = 'field-enc'`,
			want:   "every data key of purpose field-enc is retired",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, conn := newTestStore(t, provider)
			_, err := store.Load(ctx)
			require.NoError(t, err)
			_, err = store.Rotate(ctx, PurposeFieldEnc)
			require.NoError(t, err)

			_, err = conn.Exec(tc.change)
			require.NoError(t, err)
			_, err = store.Load(ctx)
			assert.ErrorContains(t, err, tc.want)
		})
	}

	t.Run("another configured KEK", func(t *testing.T) {
		store, _ := newTestStore(t, provider)
		_, err := store.Load(ctx)
		require.NoError(t, err)

		other := &Store{DB: store.DB, Provider: localProvider(t)}
		_, err = other.Load(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), provider.ID())
		assert.Contains(t, err.Error(), other.Provider.ID())
	})
}

func TestStore_RewrapMovesTheKeysToAnotherKEK(t *testing.T) {
	ctx := context.Background()
	oldKEK, newKEK := localProvider(t), localProvider(t)
	store, conn := newTestStore(t, oldKEK)
	ring, err := store.Load(ctx)
	require.NoError(t, err)
	_, err = store.Rotate(ctx, PurposeBlindIndex)
	require.NoError(t, err)
	require.NoError(t, store.Retire(ctx, PurposeBlindIndex, 1))
	before, err := ring.Current(PurposeRowMAC)
	require.NoError(t, err)

	moved := &Store{DB: conn, Provider: newKEK}
	count, err := moved.Rewrap(ctx, oldKEK)
	require.NoError(t, err)
	assert.Equal(t, len(Purposes)+1, count, "retired keys are re-wrapped too")

	ring, err = moved.Load(ctx)
	require.NoError(t, err)
	after, err := ring.Current(PurposeRowMAC)
	require.NoError(t, err)
	assert.Equal(t, fingerprint(t, before), fingerprint(t, after), "re-wrapping keeps the keys")
	_, err = store.Load(ctx)
	assert.Error(t, err, "the old KEK no longer opens the keys")

	var kekIDs int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(DISTINCT kek_id) FROM data_keys WHERE kek_id = $1`, newKEK.ID()).Scan(&kekIDs))
	assert.Equal(t, 1, kekIDs)

	_, err = moved.Rewrap(ctx, oldKEK)
	assert.ErrorContains(t, err, "was wrapped by the key-encryption key", "nothing changes when a key does not open")
}

func TestStore_WithOpenBao(t *testing.T) {
	ctx := context.Background()
	provider, _, post := openBao(t)
	store, conn := newTestStore(t, provider)

	ring, err := store.Load(ctx)
	require.NoError(t, err)
	before, err := ring.Current(PurposeFieldEnc)
	require.NoError(t, err)
	var wrapped string
	require.NoError(t, conn.QueryRow(`SELECT convert_from(wrapped_key, 'UTF8') FROM data_keys WHERE purpose = 'field-enc'`).Scan(&wrapped))
	assert.True(t, strings.HasPrefix(wrapped, "vault:v1:"), wrapped)

	// Rotate the KEK in OpenBao, re-wrap, and stop the old KEK version from
	// decrypting: the keys still load and are unchanged.
	post("keys/data-keys/rotate", "")
	count, err := store.Rewrap(ctx, provider)
	require.NoError(t, err)
	assert.Equal(t, len(Purposes), count)
	post("keys/data-keys/config", `{"min_decryption_version":2}`)

	ring, err = store.Load(ctx)
	require.NoError(t, err)
	after, err := ring.Current(PurposeFieldEnc)
	require.NoError(t, err)
	assert.Equal(t, fingerprint(t, before), fingerprint(t, after))
	require.NoError(t, conn.QueryRow(`SELECT convert_from(wrapped_key, 'UTF8') FROM data_keys WHERE purpose = 'field-enc'`).Scan(&wrapped))
	assert.True(t, strings.HasPrefix(wrapped, "vault:v2:"), wrapped)
}

// The first keys are recorded once, with the system as the actor, however
// many instances load them (#195).
func TestStore_TheFirstKeysAreAudited(t *testing.T) {
	ctx := context.Background()
	store, conn := newTestStore(t, localProvider(t))
	_, err := store.Load(ctx)
	require.NoError(t, err)
	_, err = store.Load(ctx)
	require.NoError(t, err)

	rows, err := conn.Query(`SELECT actor_type, target_id, changes->'after'->>'kek_id' FROM audit_events WHERE action = 'data_key.create' ORDER BY target_id`)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var actor, target, kek string
		require.NoError(t, rows.Scan(&actor, &target, &kek))
		assert.Equal(t, "system", actor)
		assert.Equal(t, store.Provider.ID(), kek)
		got = append(got, target)
	}
	assert.Equal(t, []string{"audit-mac/v1", "blind-index/v1", "field-enc/v1", "otp-mac/v1", "row-mac/v1"}, got)
}

// TestStore_VerifyUnwrapsWithoutCreating checks the restore-drill check of
// #236: Verify creates no key on an empty table, unwraps every key that is
// not retired with the right KEK, and fails with any other.
func TestStore_VerifyUnwrapsWithoutCreating(t *testing.T) {
	ctx := context.Background()
	provider := localProvider(t)
	store, conn := newTestStore(t, provider)

	count, err := store.Verify(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
	var rows int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Zero(t, rows, "Verify never creates a key")

	_, err = store.Load(ctx)
	require.NoError(t, err)
	count, err = store.Verify(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(Purposes), count)

	other := &Store{DB: conn, Provider: localProvider(t)}
	_, err = other.Verify(ctx)
	require.Error(t, err, "a database restored without its KEK fails the check")
}

// TestStore_LoadExistingCreatesNothing loads the keys as cmd/admin does:
// it never creates one, also not for a purpose that a newer release added
// before any instance of that release has started.
func TestStore_LoadExistingCreatesNothing(t *testing.T) {
	ctx := context.Background()
	store, conn := newTestStore(t, localProvider(t))

	_, err := store.LoadExisting(ctx)
	require.ErrorIs(t, err, ErrNoKey, "no keys at all")
	var rows int
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Zero(t, rows)

	_, err = store.Load(ctx)
	require.NoError(t, err)
	_, err = conn.Exec(`DELETE FROM data_keys WHERE purpose = $1`, PurposeOTPMAC)
	require.NoError(t, err, "as before the release that added otp-mac")
	ring, err := store.LoadExisting(ctx)
	require.NoError(t, err)
	_, err = ring.Current(PurposeFieldEnc)
	assert.NoError(t, err)
	_, err = ring.Current(PurposeOTPMAC)
	assert.ErrorIs(t, err, ErrNoKey)
	require.NoError(t, conn.QueryRow(`SELECT COUNT(*) FROM data_keys`).Scan(&rows))
	assert.Equal(t, len(Purposes)-1, rows)
}
