package idcheck

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/identitytest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	aliceID = "0012345679"
	bobID   = "0499370899"
	born    = "1991-08-16"
)

type fixture struct {
	Service
	t    *testing.T
	stub *identity.Stub
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_idcheck")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	stub := &identity.Stub{Births: map[string]string{aliceID: born, bobID: "1985-01-02"}}
	return fixture{
		Service: Service{DB: db, Inquiry: stub, Mobile: stub, Sealer: identitytest.NewSealer(),
			now: func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }},
		t: t, stub: stub,
	}
}

func (f fixture) user(u usertest.User) string {
	f.t.Helper()
	hk := usertest.Insert(f.t, f.DB, u)
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.Users, "hk = $1", hk))
	return hk
}

func (f fixture) tier(user string) string {
	f.t.Helper()
	var tier string
	require.NoError(f.t, f.DB.QueryRow(`SELECT kyc_tier FROM users WHERE hk = $1`, user).Scan(&tier))
	return tier
}

func (f fixture) events(action, outcome string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND outcome = $2`, action, outcome).Scan(&n))
	return n
}

// A national ID and its date of birth that the registry has, with the
// user's mobile number registered to it, are verified and stored sealed,
// with the date of birth (#158). Nothing that could identify the person is
// in the audit log.
func TestSubmit_VerifiesAndStores(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	f.stub.SetOwner("09121234567", aliceID)

	st, err := f.Submit(ctx, alice, aliceID, born, audit.Event{IP: "203.0.113.5"})
	require.NoError(t, err)
	assert.Equal(t, StateVerified, st.State)
	assert.Equal(t, "00******79", st.NationalID)
	assert.NotNil(t, st.VerifiedAt)

	person, has, err := identity.PersonOf(ctx, f.DB, f.Sealer, alice)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, aliceID, person.NationalID)
	assert.Equal(t, born, person.BirthDate.Format(time.DateOnly))
	assert.Equal(t, "NONE", f.tier(alice), "level 1 also needs a matched card or IBAN")
	assert.Equal(t, 1, f.events(audit.ActionUserIdentityCheck, audit.Success))

	var changes string
	require.NoError(t, f.DB.QueryRow(`SELECT changes::text FROM audit_events WHERE action = $1`, audit.ActionUserIdentityCheck).Scan(&changes))
	for _, secret := range []string{aliceID, born, "09121234567", "1991"} {
		assert.NotContains(t, changes, secret)
	}

	_, err = f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.ErrorIs(t, err, ErrAlreadyVerified)
}

// A national ID verified, with a matched card in the user's book, raises
// the user to level 1 (#158).
func TestSubmit_AMatchedCardMakesLevel1(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	// The card was matched while the national ID was stored without a date
	// of birth: the check adds it.
	require.NoError(t, identity.SaveNationalID(ctx, f.DB, f.Sealer, alice, aliceID, "stub", "", time.Now()))
	_, err := f.DB.Exec(`INSERT INTO withdrawal_destinations (id, user_hk, kind, value_enc, value_bidx, masked, verified_by, usable_from)
		VALUES (gen_random_uuid(), $1, 'card', '\x00', '\x01', '603799******0001', 'stub', NOW())`, alice)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.WithdrawalDestinations, "user_hk = $1", alice))

	_, err = f.Submit(ctx, alice, bobID, "1985-01-02", audit.Event{})
	require.ErrorIs(t, err, ErrAlreadyVerified, "a national ID of before is completed, never replaced")
	st, err := f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, StateVerified, st.State)
	assert.Equal(t, "BASIC", f.tier(alice))
	assert.Equal(t, 1, f.events(audit.ActionUserVerificationSet, audit.Success))
}

// A date of birth that does not match, or a mobile number registered to
// someone else, is rejected, finally; the attempts are limited (#158).
func TestSubmit_Mismatches(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com", Mobile: "09121234567"})
	f.stub.SetOwner("09121234567", bobID)

	st, err := f.Submit(ctx, alice, aliceID, "1991-08-17", audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, Status{State: StateRejected, NationalID: "00******79", Reason: checkMismatched, AttemptsLeft: MaxFailures - 1}, st)
	st, err = f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, StateRejected, st.State, "the mobile number is not hers")
	_, has, err := identity.PersonOf(ctx, f.DB, f.Sealer, alice)
	require.NoError(t, err)
	assert.False(t, has)

	for range MaxFailures - 2 {
		_, err := f.Submit(ctx, alice, aliceID, "1990-01-01", audit.Event{})
		require.NoError(t, err)
	}
	_, err = f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.ErrorIs(t, err, ErrTooManyAttempts, "national IDs cannot be tried one by one")

	var problems Invalid
	_, err = f.Submit(ctx, alice, "1111111111", "16/08/1991", audit.Event{})
	require.ErrorAs(t, err, &problems)
	assert.Contains(t, problems, "national_id")
	assert.Contains(t, problems, "birth_date")
}

// One person, one account (#158): a national ID verified for another
// account is refused, and the refusal counts as a failure.
func TestSubmit_OnePersonOneAccount(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	again := f.user(usertest.User{Email: "alice2@example.com"})
	_, err := f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.NoError(t, err)

	calls := f.stub.Calls
	st, err := f.Submit(ctx, again, aliceID, born, audit.Event{})
	require.ErrorIs(t, err, ErrNationalIDTaken)
	assert.Equal(t, StateRejected, st.State)
	assert.Equal(t, checkTaken, st.Reason)
	assert.Equal(t, calls, f.stub.Calls, "the provider is not asked")
	assert.Equal(t, 1, f.events(audit.ActionUserIdentityCheck, audit.Failure))
	_, has, err := identity.PersonOf(ctx, f.DB, f.Sealer, again)
	require.NoError(t, err)
	assert.False(t, has)

	// Two accounts at once: the unique blind index lets one through.
	bobA, bobB := f.user(usertest.User{Email: "bob@example.com"}), f.user(usertest.User{Email: "bob2@example.com"})
	errs := make(chan error, 2)
	for _, u := range []string{bobA, bobB} {
		go func() { _, err := f.Submit(ctx, u, bobID, "1985-01-02", audit.Event{}); errs <- err }()
	}
	var ok, refused int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrNationalIDTaken):
			refused++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, refused)
}

// A provider that cannot answer leaves the check pending, approving
// nothing; the scheduler asks again, waiting longer each time, and applies
// the answer when it comes (#158). A check marked matched in SQL is
// refused.
func TestSubmit_PendingWhileTheProviderIsDown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	f.stub.SetUnavailable(true)

	st, err := f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, StatePending, st.State)
	_, err = f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.ErrorIs(t, err, ErrPending, "one at a time")

	due := func() {
		_, err := f.DB.Exec(`UPDATE identity_checks SET next_attempt_at = NOW() WHERE status = 'pending'`)
		require.NoError(t, err)
	}
	due()
	_, err = f.RetryDue(ctx, 10)
	require.NoError(t, err)
	var attempts int
	var next time.Time
	require.NoError(t, f.DB.QueryRow(`SELECT attempts, next_attempt_at FROM identity_checks WHERE user_hk = $1`, alice).Scan(&attempts, &next))
	assert.Equal(t, 2, attempts)
	assert.True(t, next.After(time.Now().Add(90*time.Second)), "two minutes after the second failure")
	_, has, err := identity.PersonOf(ctx, f.DB, f.Sealer, alice)
	require.NoError(t, err)
	assert.False(t, has, "nothing approved")

	f.stub.SetUnavailable(false)
	due()
	_, err = f.RetryDue(ctx, 10)
	require.NoError(t, err)
	st, err = f.Status(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, StateVerified, st.State)

	// Bob's pending check marked matched in SQL verifies nothing.
	bob := f.user(usertest.User{Email: "bob@example.com"})
	f.stub.SetUnavailable(true)
	_, err = f.Submit(ctx, bob, bobID, "1985-01-02", audit.Event{})
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE identity_checks SET status = 'matched', decided_at = NOW(), next_attempt_at = NULL WHERE user_hk = $1`, bob)
	require.NoError(t, err)
	_, err = f.Status(ctx, bob)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	_, has, err = identity.PersonOf(ctx, f.DB, f.Sealer, bob)
	require.NoError(t, err)
	assert.False(t, has)
}

// A national ID swapped in SQL for another one is refused (#158, #139).
func TestPersonOf_ANationalIDChangedInSQL(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(usertest.User{Email: "alice@example.com"})
	_, err := f.Submit(ctx, alice, aliceID, born, audit.Event{})
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE user_national_ids SET verified_at = verified_at - INTERVAL '1 year' WHERE user_hk = $1`, alice)
	require.NoError(t, err)
	_, _, err = identity.PersonOf(ctx, f.DB, f.Sealer, alice)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	var findings int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'user_national_ids'`).Scan(&findings))
	assert.Equal(t, 1, findings)
}
