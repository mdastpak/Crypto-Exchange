package identity_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/testutil/identitytest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_identity"

const nationalID = "0012345679"

func TestValidNationalID(t *testing.T) {
	for _, id := range []string{"0012345679", "1234567891", "9876543210"} {
		assert.True(t, identity.ValidNationalID(id), id)
	}
	for _, id := range []string{"", "001234567", "00123456790", "0012345678", "1111111111", "00123456a9", "۰۰۱۲۳۴۵۶۷۹"} {
		assert.False(t, identity.ValidNationalID(id), id)
	}
}

func TestNationalID_IsSealedAndBoundToItsUser(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Migrated(t, testDBName)
	sealer := identitytest.NewSealer()
	user := usertest.Insert(t, db, usertest.User{Email: "national@example.com"})
	other := usertest.Insert(t, db, usertest.User{Email: "no-national@example.com"})

	_, has, err := identity.NationalID(ctx, db, sealer, user)
	require.NoError(t, err)
	assert.False(t, has)

	require.NoError(t, identity.SaveNationalID(ctx, db, sealer, user, nationalID, "jibit", "ref-1", time.Now()))
	got, has, err := identity.NationalID(ctx, db, sealer, user)
	require.NoError(t, err)
	assert.True(t, has)
	assert.Equal(t, nationalID, got)

	var stored []byte
	require.NoError(t, db.QueryRow(`SELECT national_id_sealed FROM user_national_ids WHERE user_hk = $1`, user).Scan(&stored))
	assert.False(t, bytes.Contains(stored, []byte(nationalID)), "the database never holds it in the clear")

	// A sealed value copied to another user does not open there.
	_, err = db.Exec(`INSERT INTO user_national_ids (user_hk, national_id_sealed, verified_at, provider) VALUES ($1, $2, NOW(), 'copy')`, other, stored)
	require.NoError(t, err)
	_, _, err = identity.NationalID(ctx, db, sealer, other)
	assert.Error(t, err)

	_, _, err = identity.NationalID(ctx, db, nil, user)
	assert.ErrorIs(t, err, identity.ErrNoKeys, "a national ID without keys is never skipped")

	require.NoError(t, identity.SaveNationalID(ctx, db, sealer, user, "9876543210", "jibit", "", time.Now()), "a new verification replaces it")
	got, _, err = identity.NationalID(ctx, db, sealer, user)
	require.NoError(t, err)
	assert.Equal(t, "9876543210", got)
	assert.Error(t, identity.SaveNationalID(ctx, db, sealer, user, "1111111111", "jibit", "", time.Now()), "an invalid ID is refused")

	// A malformed ID sealed for the user outside the backend is checked
	// again when it is read, and never used.
	forged, err := sealer.SealField([]byte("user_national_ids.national_id_sealed:"+user), []byte("1234567890"))
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE user_national_ids SET national_id_sealed = $2 WHERE user_hk = $1`, user, forged)
	require.NoError(t, err)
	_, _, err = identity.NationalID(ctx, db, sealer, user)
	assert.ErrorIs(t, err, identity.ErrInvalidNationalID)
}

func TestStub(t *testing.T) {
	ctx := context.Background()
	s := &identity.Stub{}
	s.SetOwner("09121234567", nationalID)
	m, err := s.Match(ctx, nationalID, "09121234567")
	require.NoError(t, err)
	assert.True(t, m.Matched)
	assert.NotEmpty(t, m.Ref)
	m, err = s.Match(ctx, "9876543210", "09121234567")
	require.NoError(t, err)
	assert.False(t, m.Matched)
	m, err = s.Match(ctx, nationalID, "09129999999")
	require.NoError(t, err)
	assert.False(t, m.Matched, "unlisted numbers do not match by default")
	s.MatchUnlisted = true
	m, _ = s.Match(ctx, nationalID, "09129999999")
	assert.True(t, m.Matched)
	s.SetUnavailable(true)
	_, err = s.Match(ctx, nationalID, "09121234567")
	assert.ErrorIs(t, err, identity.ErrUnavailable)
}
