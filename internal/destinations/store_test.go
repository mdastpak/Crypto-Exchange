package destinations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/identitytest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const trc20 = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// fixture is a store on an empty test database, with row seals and field
// encryption on, and a stub provider of bank ownership.
type fixture struct {
	Store
	stub *identity.Stub
	t    *testing.T
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_destinations")
	sealtest.Use(t, conn, rowseal.ModeEnforce)
	policy, err := verification.NewPolicy(config.VerificationConfig{})
	require.NoError(t, err)
	stub := &identity.Stub{}
	return fixture{
		Store: Store{DB: conn, Levels: verification.Store{DB: conn, Policy: policy},
			Ownership: stub, NationalIDs: identitytest.NewSealer()},
		stub: stub, t: t,
	}
}

// user creates a user at a level, with a verified national ID when one is
// given.
func (f fixture) user(tier shared.KYCTier, nationalID string) string {
	f.t.Helper()
	var hk string
	require.NoError(f.t, f.DB.QueryRow(`INSERT INTO users (status, kyc_tier, kyc_status) VALUES ('ACTIVE', $1, 'APPROVED') RETURNING hk::text`,
		tier).Scan(&hk))
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.Users, "hk = $1", hk))
	if nationalID != "" {
		require.NoError(f.t, identity.Save(context.Background(), f.DB, f.NationalIDs, hk, identity.Verified{
			Person:   identity.Person{NationalID: nationalID, BirthDate: time.Date(1990, 3, 21, 0, 0, 0, 0, time.UTC)},
			Provider: "stub", At: time.Now()}))
	}
	return hk
}

func (f fixture) events(action, outcome string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND outcome = $2`, action, outcome).Scan(&n))
	return n
}

func TestStore_FiatDestinationsOfTheUsersOwn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(shared.KYCTierBasic, "0012345679")
	bob := f.user(shared.KYCTierBasic, "0499370899")
	iban := makeIBAN("0540102680020817909002")
	f.stub.SetOwner(iban, "0012345679")

	d, err := f.Add(ctx, alice, Request{Kind: KindIBAN, Number: iban, Label: "salary"}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, KindIBAN, d.Kind)
	assert.Equal(t, "parsian", d.Bank)
	assert.Equal(t, "salary", d.Label)
	assert.NotContains(t, d.Masked, iban[7:22], "the middle is hidden")
	assert.WithinDuration(t, d.CreatedAt, d.UsableFrom, time.Second, "fiat destinations wait for nothing by default")
	assert.Equal(t, 1, f.events(audit.ActionUserDestinationAdd, audit.Success))

	// Stored encrypted only: neither the IBAN nor its digits are in the row.
	var raw string
	require.NoError(t, f.DB.QueryRow(`SELECT encode(value_enc, 'escape') || masked FROM withdrawal_destinations WHERE id = $1`, d.ID).Scan(&raw))
	assert.NotContains(t, raw, iban)

	_, err = f.Add(ctx, alice, Request{Kind: KindIBAN, Number: iban}, audit.Event{})
	require.ErrorIs(t, err, ErrExists)
	f.stub.SetOwner(iban, "0499370899")
	_, err = f.Add(ctx, bob, Request{Kind: KindIBAN, Number: iban}, audit.Event{})
	require.ErrorIs(t, err, ErrTaken, "a card, IBAN, or account is in one book at a time")

	// Someone else's card: the provider says no, and the attempt is audited.
	other := luhn("610433123456789")
	f.stub.SetOwner(other, "0499370899")
	_, err = f.Add(ctx, alice, Request{Kind: KindCard, Number: other}, audit.Event{})
	require.ErrorIs(t, err, ErrNotOwned)
	assert.Equal(t, 1, f.events(audit.ActionUserDestinationAdd, audit.Failure))

	// No national ID, no provider, a provider that does not answer.
	carol := f.user(shared.KYCTierBasic, "")
	_, err = f.Add(ctx, carol, Request{Kind: KindCard, Number: other}, audit.Event{})
	require.ErrorIs(t, err, ErrNationalIDRequired)
	noProvider := f.Store
	noProvider.Ownership = nil
	_, err = noProvider.Add(ctx, alice, Request{Kind: KindCard, Number: luhn("603799000000001")}, audit.Event{})
	require.ErrorIs(t, err, ErrOwnershipUnavailable, "no provider: nothing would ever check it")
	f.stub.SetUnavailable(true)
	pending, err := f.Add(ctx, alice, Request{Kind: KindCard, Number: luhn("603799000000001")}, audit.Event{})
	require.NoError(t, err, "a provider that does not answer: added pending (#158)")
	assert.True(t, pending.OwnershipPending)
	f.stub.SetUnavailable(false)
	_, err = f.Usable(ctx, f.DB, alice, pending.ID)
	require.ErrorIs(t, err, ErrOwnershipPending, "not usable while pending")

	list, err := f.List(ctx, alice)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, pending.ID, list[0].ID, "the newest first")
	assert.Equal(t, d.ID, list[1].ID)

	resolved, err := f.Usable(ctx, f.DB, alice, d.ID)
	require.NoError(t, err)
	assert.Equal(t, iban, resolved.Value, "the payout reads the number in the clear")
	_, err = f.Usable(ctx, f.DB, bob, d.ID)
	require.ErrorIs(t, err, ErrNotFound, "not another user's")
}

func TestStore_CryptoDestinationsWaitAndNeedTheirLevel(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	basic := f.user(shared.KYCTierBasic, "")
	full := f.user(shared.KYCTierStandard, "")

	_, err := f.Add(ctx, basic, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	var refusal *verification.Refusal
	require.ErrorAs(t, err, &refusal, "crypto withdrawals need level 2 by default, so do their destinations")

	d, err := f.Add(ctx, full, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, "tron", d.Network)
	assert.InDelta(t, 24*time.Hour, d.UsableFrom.Sub(d.CreatedAt), float64(time.Second), "an address waits 24 hours by default")

	_, err = f.Usable(ctx, f.DB, full, d.ID)
	var waiting *NotUsableYet
	require.ErrorAs(t, err, &waiting)
	require.ErrorIs(t, err, ErrNotUsableYet)
	assert.Equal(t, d.UsableFrom, waiting.Until)

	// The same address again is refused; for another user it is allowed.
	_, err = f.Add(ctx, full, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.ErrorIs(t, err, ErrExists)
	other := f.user(shared.KYCTierStandard, "")
	_, err = f.Add(ctx, other, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.NoError(t, err, "an address may be in several books")

	// After the wait, it is usable.
	_, err = f.DB.Exec(`UPDATE withdrawal_destinations SET usable_from = NOW() - interval '1 second' WHERE id = $1`, d.ID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.WithdrawalDestinations, "id = $1", d.ID))
	resolved, err := f.Usable(ctx, f.DB, full, d.ID)
	require.NoError(t, err)
	assert.Equal(t, trc20, resolved.Value)

	// Removed: gone from the book and unusable, and it can be added again.
	require.NoError(t, f.Remove(ctx, full, d.ID, audit.Event{}))
	require.ErrorIs(t, f.Remove(ctx, full, d.ID, audit.Event{}), ErrNotFound)
	_, err = f.Usable(ctx, f.DB, full, d.ID)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 1, f.events(audit.ActionUserDestinationRemove, audit.Success))
	_, err = f.Add(ctx, full, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.NoError(t, err)
}

// TestStore_ADestinationChangedInSQLIsNotPaid swaps the address of a
// destination outside the backend, and checks that it is neither listed
// nor usable.
func TestStore_ADestinationChangedInSQLIsNotPaid(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(shared.KYCTierStandard, "")
	mallory := f.user(shared.KYCTierStandard, "")
	mine, err := f.Add(ctx, alice, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.NoError(t, err)
	theirs, err := f.Add(ctx, mallory, Request{Kind: KindCrypto, Network: "bitcoin", Address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}, audit.Event{})
	require.NoError(t, err)

	_, err = f.DB.Exec(`UPDATE withdrawal_destinations SET value_enc = (SELECT value_enc FROM withdrawal_destinations WHERE id = $2),
		usable_from = NOW() - interval '1 hour' WHERE id = $1`, mine.ID, theirs.ID)
	require.NoError(t, err)

	list, err := f.List(ctx, alice)
	require.NoError(t, err)
	assert.Empty(t, list, "a destination that fails its seal is left out")
	_, err = f.Usable(ctx, f.DB, alice, mine.ID)
	require.ErrorIs(t, err, ErrUnverified)
	var findings int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'withdrawal_destinations' AND row_key = $1`,
		mine.ID).Scan(&findings))
	assert.Equal(t, 1, findings)
}

func TestStore_TheBookHasLimitsAndTheHoldApplies(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.Settings.MaxDestinations = 2
	alice := f.user(shared.KYCTierStandard, "")
	for _, address := range []string{trc20, "TJCnKsPa7y5okkXvQAidZBzqx3QyQ6sxMW"} {
		_, err := f.Add(ctx, alice, Request{Kind: KindCrypto, Network: "tron", Address: address}, audit.Event{})
		require.NoError(t, err, address)
	}
	_, err := f.Add(ctx, alice, Request{Kind: KindCrypto, Network: "bitcoin", Address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}, audit.Event{})
	require.ErrorIs(t, err, ErrTooMany)

	// A change of a security setting holds new destinations (#251).
	bob := f.user(shared.KYCTierStandard, "")
	tx, err := f.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = restrictions.StartSecurityHold(ctx, tx, bob, time.Hour)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = f.Add(ctx, bob, Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	var held *restrictions.Refusal
	require.True(t, errors.As(err, &held), "%v", err)
	assert.Equal(t, restrictions.CodeSecurityHold, held.Code)

	_, err = f.Add(ctx, alice, Request{Kind: KindCard, Number: "1234"}, audit.Event{})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorIs(t, f.Remove(ctx, alice, "not-an-id", audit.Event{}), ErrNotFound)
}

func TestStore_NeedsDataKeys(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_destinations")
	s := Store{DB: conn}
	_, err := s.Add(context.Background(), shared.NewUUID().String(), Request{Kind: KindCrypto, Network: "tron", Address: trc20}, audit.Event{})
	require.ErrorIs(t, err, ErrNoKeys)
}

// TestStore_ConcurrentAdditionsKeepTheLimit adds many addresses to one
// list at once: exactly as many as the list holds are added.
func TestStore_ConcurrentAdditionsKeepTheLimit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.Settings.MaxDestinations = 3
	alice := f.user(shared.KYCTierStandard, "")
	const attempts = 10
	results := make(chan error, attempts)
	for range attempts {
		go func() {
			address := make([]byte, 20)
			_, _ = rand.Read(address)
			_, err := f.Add(ctx, alice, Request{Kind: KindCrypto, Network: "ethereum", Address: "0x" + hex.EncodeToString(address)}, audit.Event{})
			results <- err
		}()
	}
	added, full := 0, 0
	for range attempts {
		err := <-results
		switch {
		case err == nil:
			added++
		case errors.Is(err, ErrTooMany):
			full++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 3, added)
	assert.Equal(t, attempts-3, full)
	list, err := f.List(ctx, alice)
	require.NoError(t, err)
	assert.Len(t, list, 3)
}

// TestStore_AStoredNumberIsCheckedAgainBeforeUse stores a card number that
// fails its check digit, encrypted and sealed as the backend would, and
// checks that no withdrawal can use it.
func TestStore_AStoredNumberIsCheckedAgainBeforeUse(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(shared.KYCTierBasic, "0012345679")
	card := luhn("603799123456789")
	f.stub.SetOwner(card, "0012345679")
	d, err := f.Add(ctx, alice, Request{Kind: KindCard, Number: card}, audit.Event{})
	require.NoError(t, err)
	_, err = f.Usable(ctx, f.DB, alice, d.ID)
	require.NoError(t, err)

	wrong := card[:15] + fmt.Sprint((int(card[15]-'0')+1)%10)
	sealed, err := field.Active().Seal(place(d.ID, "value_enc"), wrong)
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE withdrawal_destinations SET value_enc = $2 WHERE id = $1`, d.ID, sealed)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.WithdrawalDestinations, "id = $1", d.ID))

	_, err = f.Usable(ctx, f.DB, alice, d.ID)
	require.ErrorIs(t, err, ErrUnverified)
}

// A card added while the provider could not answer is pending (#158): the
// scheduler asks again, outside any transaction, and decides with the
// user's row locked first. A match verifies it and, with the verified
// national ID, raises the user to level 1; a mismatch removes it; another
// failure waits longer. Nothing is approved on a failure, and a row
// changed in SQL is refused.
func TestStore_PendingOwnershipChecks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user(shared.KYCTierNone, "0012345679")
	f.stub.SetOwner(luhn("603799000000001"), "0012345679")
	f.stub.SetUnavailable(true)
	mine, err := f.Add(ctx, alice, Request{Kind: KindCard, Number: luhn("603799000000001")}, audit.Event{})
	require.NoError(t, err)
	other := makeIBAN("0540102680020817909002")
	theirs, err := f.Add(ctx, alice, Request{Kind: KindIBAN, Number: other}, audit.Event{})
	require.NoError(t, err)
	f.stub.SetOwner(other, "0499370899")
	level := func() string {
		var tier string
		require.NoError(t, f.DB.QueryRow(`SELECT kyc_tier FROM users WHERE hk = $1`, alice).Scan(&tier))
		return tier
	}
	due := func() {
		_, err := f.DB.Exec(`UPDATE withdrawal_destinations SET ownership_next_check = NOW() WHERE ownership_pending_since IS NOT NULL`)
		require.NoError(t, err)
	}

	// Still unavailable: both wait longer, and nothing is approved.
	result, err := f.CheckPending(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, CheckResult{}, result, "not due yet")
	due()
	result, err = f.CheckPending(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, result.StillPending)
	var attempts int
	var next time.Time
	require.NoError(t, f.DB.QueryRow(`SELECT ownership_attempts, ownership_next_check FROM withdrawal_destinations WHERE id = $1`,
		mine.ID).Scan(&attempts, &next))
	assert.Equal(t, 1, attempts)
	assert.True(t, next.After(time.Now().Add(30*time.Second)), "a minute later")
	assert.Equal(t, string(shared.KYCTierNone), level())

	// The provider answers: the card matches, the IBAN is someone else's.
	f.stub.SetUnavailable(false)
	due()
	result, err = f.CheckPending(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, CheckResult{Matched: 1, NotOwned: 1}, result)
	resolved, err := f.Resolve(ctx, f.DB, alice, mine.ID)
	require.NoError(t, err)
	assert.False(t, resolved.OwnershipPending)
	list, err := f.List(ctx, alice)
	require.NoError(t, err)
	require.Len(t, list, 1, "the IBAN that is not hers is removed")
	assert.Equal(t, mine.ID, list[0].ID)
	_, err = f.Usable(ctx, f.DB, alice, theirs.ID)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, string(shared.KYCTierBasic), level(), "a verified national ID and a matched card are level 1")
	assert.Equal(t, 1, f.events(audit.ActionDestinationOwnershipCheck, audit.Success))
	assert.Equal(t, 1, f.events(audit.ActionDestinationOwnershipCheck, audit.Failure))
	assert.Equal(t, 1, f.events(audit.ActionUserVerificationSet, audit.Success))

	// A pending card cleared in SQL, to pass for verified, does not match
	// its seal: it is not used, and the scheduler leaves it alone.
	f.stub.SetUnavailable(true)
	forged, err := f.Add(ctx, alice, Request{Kind: KindCard, Number: luhn("603799000000002")}, audit.Event{})
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE withdrawal_destinations SET ownership_pending_since = NULL, ownership_next_check = NULL,
		verified_by = 'stub' WHERE id = $1`, forged.ID)
	require.NoError(t, err)
	_, err = f.Usable(ctx, f.DB, alice, forged.ID)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
}

// A card matched at once raises a level-0 user with a verified national ID
// to level 1, and never lowers or touches a higher level (#158).
func TestStore_AMatchedCardGrantsLevel1(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	tier := func(user string) string {
		var t2 string
		require.NoError(t, f.DB.QueryRow(`SELECT kyc_tier FROM users WHERE hk = $1`, user).Scan(&t2))
		return t2
	}
	alice := f.user(shared.KYCTierNone, "0012345679")
	f.stub.SetOwner(luhn("603799000000003"), "0012345679")
	f.stub.SetOwner(luhn("603799000000004"), "0499370899")
	_, err := f.Add(ctx, alice, Request{Kind: KindCard, Number: luhn("603799000000003")}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, string(shared.KYCTierBasic), tier(alice))

	bob := f.user(shared.KYCTierStandard, "0499370899")
	_, err = f.Add(ctx, bob, Request{Kind: KindCard, Number: luhn("603799000000004")}, audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, string(shared.KYCTierStandard), tier(bob), "level 2 stays")
}
