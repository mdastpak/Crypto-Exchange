package rowseal_test

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyAll_FindsADeletedPassword gives two accounts a users row that
// records a password (#337), deletes the password row of one in SQL, and
// expects the verifier to record a row_missing finding for that one only,
// in both modes: report mode cannot seal what is not there.
func TestVerifyAll_FindsADeletedPassword(t *testing.T) {
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			s := e.sealer(mode)
			ctx := context.Background()
			withPassword := func() string {
				hk := e.newUser(t)
				_, err := e.conn.Exec(`INSERT INTO user_passwords (user_hk, password_hash) VALUES ($1, '$argon2id$v=19$test')`, hk)
				require.NoError(t, err)
				require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.UserPasswords, "user_hk = $1", hk))
				_, err = e.conn.Exec(`UPDATE users SET password_set_at = NOW() WHERE hk = $1`, hk)
				require.NoError(t, err)
				require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.Users, "hk = $1", hk))
				return hk
			}
			kept, deleted := withPassword(), withPassword()
			_, err := e.conn.Exec(`DELETE FROM user_passwords WHERE user_hk = $1`, deleted)
			require.NoError(t, err)

			report, err := s.VerifyAll(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, report.Findings)
			findings := e.openFindings(t)
			require.Len(t, findings, 1)
			assert.Equal(t, "user_passwords", findings[0].Table)
			assert.Equal(t, deleted, findings[0].RowKey)
			assert.Equal(t, rowseal.CheckRowMissing, findings[0].Check)
			assert.Equal(t, 1, e.events(t, "integrity.seal_mismatch"), "the first detection is audited")
			assert.NotEqual(t, kept, findings[0].RowKey)

			// A second pass counts the detection on the open finding.
			_, err = s.VerifyAll(ctx)
			require.NoError(t, err)
			findings = e.openFindings(t)
			require.Len(t, findings, 1)
			assert.EqualValues(t, 2, findings[0].SeenCount)
		})
	}
}

// TestCheck_ARecordedPasswordDoesNotBreakTheSeal checks a users row that
// records a password, sealed by the backend, and the same row after the
// record was set back to NULL in SQL.
func TestCheck_ARecordedPasswordDoesNotBreakTheSeal(t *testing.T) {
	e := newEnv(t)
	e.sealer(rowseal.ModeEnforce)
	ctx := context.Background()
	hk := e.newUser(t)
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)), "no record: the seal of the older rows")

	_, err := e.conn.Exec(`UPDATE users SET password_set_at = NOW() WHERE hk = $1`, hk)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, e.conn, rowseal.Users, "hk = $1", hk))
	require.NoError(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)))

	_, err = e.conn.Exec(`UPDATE users SET password_set_at = NULL WHERE hk = $1`, hk)
	require.NoError(t, err)
	assert.ErrorIs(t, rowseal.Check(ctx, rowseal.Users, e.readUser(t, hk)), rowseal.ErrUnverified)
}
