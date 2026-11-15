package withdrawals

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/identitytest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/userauth"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authenticator is an authenticator app that accepts one code.
type authenticator struct{}

func (authenticator) Enabled(context.Context, string) (bool, error) { return true, nil }

func (authenticator) Verify(_ context.Context, _, code string) error {
	if code != "123456" {
		return userauth.ErrWrongTOTP
	}
	return nil
}

type fixture struct {
	Service
	t       *testing.T
	confirm confirm.Service
	user    string
	usdt    string
	address string
}

var origin = audit.Event{IP: "203.0.113.7"}

// newFixture imports the registry, and gives a user 1000 USDT and a TRON
// address in their book. Crypto withdrawals are allowed at every level,
// with a daily limit of USDT, none when empty.
func newFixture(t *testing.T, daily string) fixture {
	t.Helper()
	ctx := context.Background()
	db := pgtest.Migrated(t, "testdb_withdrawals")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)

	policy, err := verification.NewPolicy(config.VerificationConfig{
		MinLevel:         map[string]int{"withdraw_crypto": 0, "withdraw_fiat": 0},
		WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{"USDT": {0: {Daily: daily}}},
	})
	require.NoError(t, err)
	levels := verification.Store{DB: db, Policy: policy}
	noWait := 0
	book := destinations.Store{DB: db, Levels: levels, Settings: config.WithdrawalsConfig{MaxDestinations: 20, CryptoDestinationWaitHours: &noWait},
		Ownership: &identity.Stub{MatchUnlisted: true}, NationalIDs: identitytest.NewSealer()}

	user := usertest.Insert(t, db, usertest.User{Email: "alice@example.com"})
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.Users, "hk = $1", user))
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.UserContacts, "user_hk = $1", user))
	d, err := book.Add(ctx, user, destinations.Request{Kind: destinations.KindCrypto, Network: "tron",
		Address: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"}, origin)
	require.NoError(t, err)

	var usdt string
	require.NoError(t, db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&usdt))
	l := ledger.Ledger{DB: db}
	deposit := ledger.Amount{Currency: uuidOf(usdt), Value: shared.NewDecimalFromInt64(1000)}
	_, err = l.DetectDeposit(ctx, "deposit-1:detect", account(user), deposit, ledger.Ref{Type: "deposit", ID: "1"})
	require.NoError(t, err)
	_, err = l.CreditDeposit(ctx, "deposit-1:credit", account(user), deposit, ledger.Ref{Type: "deposit", ID: "1"})
	require.NoError(t, err)

	c := confirm.Service{DB: db, Authenticator: authenticator{}, MaxAttempts: 3}
	return fixture{
		Service: Service{DB: db, Confirm: c, Destinations: book, Levels: levels, Ledger: l,
			ReviewAbove: map[string]shared.Decimal{"USDT": shared.NewDecimalFromInt64(300)}},
		t: t, confirm: c, user: user, usdt: usdt, address: d.ID,
	}
}

func (f fixture) request(amount, key string) Request {
	a, err := shared.NewDecimalFromString(amount)
	require.NoError(f.t, err)
	return Request{CurrencyHK: f.usdt, Network: "tron", DestinationID: f.address, Amount: a, IdempotencyKey: key}
}

// withdraw starts the request, confirms it, and runs it.
func (f fixture) withdraw(r Request) (Withdrawal, error) {
	f.t.Helper()
	ctx := context.Background()
	out, err := f.Request(ctx, f.user, r, "client", origin)
	if err != nil {
		return Withdrawal{}, err
	}
	require.NotNil(f.t, out.Challenge)
	require.NoError(f.t, f.confirm.Confirm(ctx, f.user, out.Challenge.ID, confirm.Proof{Code: "123456"}, origin))
	r.ConfirmationID = out.Challenge.ID
	out, err = f.Request(ctx, f.user, r, "client", origin)
	if err != nil {
		return Withdrawal{}, err
	}
	require.NotNil(f.t, out.Withdrawal)
	return *out.Withdrawal, nil
}

func (f fixture) balance() (available, locked string) {
	f.t.Helper()
	b, err := f.Ledger.Balance(context.Background(), account(f.user), uuidOf(f.usdt))
	require.NoError(f.t, err)
	return b.Available.String(), b.Locked.String()
}

func TestRequest_ConfirmsThenLocksTheFunds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	r := f.request("100", "key-1")

	out, err := f.Request(ctx, f.user, r, "client", origin)
	require.NoError(t, err)
	require.NotNil(t, out.Challenge, "a confirmation first")
	assert.Nil(t, out.Withdrawal)
	assert.Equal(t, confirm.MethodTOTP, out.Challenge.Method)
	assert.Equal(t, "100 USDT", out.Challenge.Details.Amount)
	assert.Equal(t, "TEkxiT...dz8", out.Challenge.Details.Destination[:6]+"..."+out.Challenge.Details.Destination[len(out.Challenge.Details.Destination)-3:])
	available, _ := f.balance()
	assert.Equal(t, "1000", available, "nothing is locked before the confirmation")

	r.ConfirmationID = out.Challenge.ID
	_, err = f.Request(ctx, f.user, r, "client", origin)
	require.ErrorIs(t, err, confirm.ErrNotConfirmed)

	require.NoError(t, f.confirm.Confirm(ctx, f.user, out.Challenge.ID, confirm.Proof{Code: "123456"}, origin))
	out, err = f.Request(ctx, f.user, r, "client", origin)
	require.NoError(t, err)
	w := *out.Withdrawal
	assert.Equal(t, StatusQueued, w.Status)
	assert.Equal(t, "USDT", w.Currency)
	assert.Equal(t, "100", w.Amount.String())
	assert.Equal(t, "1", w.Fee.String(), "the fee of the network")
	assert.Equal(t, "101", w.Total.String())
	available, locked := f.balance()
	assert.Equal(t, "899", available)
	assert.Equal(t, "101", locked)

	t.Run("a retry of the key returns the same withdrawal", func(t *testing.T) {
		again, err := f.Request(ctx, f.user, r, "client", origin)
		require.NoError(t, err)
		assert.True(t, again.Replayed)
		assert.Equal(t, w.ID, again.Withdrawal.ID)
		_, locked := f.balance()
		assert.Equal(t, "101", locked, "locked once")
	})

	t.Run("the key with another request", func(t *testing.T) {
		other := f.request("200", "key-1")
		_, err := f.Request(ctx, f.user, other, "client", origin)
		require.ErrorIs(t, err, ErrIdempotencyConflict)
	})

	t.Run("a confirmation confirms its request only", func(t *testing.T) {
		other := f.request("100", "key-2")
		other.ConfirmationID = out.Withdrawal.ID
		_, err := f.Request(ctx, f.user, other, "client", origin)
		require.Error(t, err)
		other.ConfirmationID = r.ConfirmationID
		_, err = f.Request(ctx, f.user, other, "client", origin)
		require.ErrorIs(t, err, confirm.ErrMismatch, "bound to the key, the amount, and the destination")
	})

	got, err := f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, w.ID, got.ID)
	assert.Empty(t, got.UserHK)
	_, err = f.Get(ctx, usertest.Insert(t, f.DB, usertest.User{Email: "bob@example.com"}), w.ID)
	require.ErrorIs(t, err, ErrNotFound, "not another user's")
	var events int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND target_id = $2`,
		audit.ActionUserWithdrawalRequest, w.ID).Scan(&events))
	assert.Equal(t, 1, events)
}

func TestRequest_RefusesWhatCannotBePaid(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	refused := func(r Request) error {
		_, err := f.Request(ctx, f.user, r, "client", origin)
		return err
	}

	err := refused(f.request("10", "a"))
	var problems Invalid
	require.ErrorAs(t, err, &problems)
	assert.Contains(t, problems["amount"], "below the minimum of 20")
	require.ErrorAs(t, refused(f.request("20.0000001", "b")), &problems)
	assert.Contains(t, problems["amount"], "decimals")
	require.ErrorIs(t, refused(f.request("1000", "c")), ErrInsufficientBalance, "with the fee, above the balance")

	r := f.request("100", "d")
	r.Network = "ethereum"
	require.ErrorIs(t, refused(r), ErrClosed, "withdrawals of USDT on ethereum are closed in the registry")
	r.Network = "bitcoin"
	require.ErrorAs(t, refused(r), &problems)
	assert.Contains(t, problems["network"], "not a network of USDT")

	// The tron network closed.
	_, err = f.DB.Exec(`UPDATE currency_networks SET withdrawal_enabled = false WHERE currency_hk = $1 AND network = 'tron'`, f.usdt)
	require.NoError(t, err)
	require.ErrorIs(t, refused(f.request("100", "e")), rowseal.ErrUnverified, "a network changed in SQL is not used")

	var none int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM operation_confirmations`).Scan(&none))
	assert.Zero(t, none, "no code was sent for any of them")
}

func TestRequest_KeepsTheDailyLimitAndTheRestrictions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "600")
	_, err := f.withdraw(f.request("100", "first"))
	require.NoError(t, err)
	_, err = f.withdraw(f.request("298", "second"))
	require.NoError(t, err, "399 of 600 with the fees")

	var refusal *verification.Refusal
	_, err = f.Request(ctx, f.user, f.request("250", "third"), "client", origin)
	require.ErrorAs(t, err, &refusal, "above the daily limit with the two before")

	tx, err := f.DB.Begin()
	require.NoError(t, err)
	_, err = restrictions.StartSecurityHold(ctx, tx, f.user, time.Hour)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var held *restrictions.Refusal
	_, err = f.Request(ctx, f.user, f.request("50", "fourth"), "client", origin)
	require.ErrorAs(t, err, &held)
	assert.Equal(t, restrictions.CodeSecurityHold, held.Code)
}

func TestReview_CancelAndReject_ReleaseTheFunds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	admin := usertest.Insert(t, f.DB, usertest.User{Email: "admin@example.com"})

	held, err := f.withdraw(f.request("300", "big"))
	require.NoError(t, err)
	assert.Equal(t, StatusReview, held.Status, "at review_above")
	assert.Equal(t, ReviewAmount, held.ReviewReason)
	review, err := f.Review(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, review, 1)
	assert.Equal(t, f.user, review[0].UserHK, "the administrators see whose it is")

	approved, err := f.Approve(ctx, held.ID, admin, origin)
	require.NoError(t, err)
	assert.Equal(t, StatusQueued, approved.Status)
	assert.NotNil(t, approved.ReviewedAt)
	_, err = f.Approve(ctx, held.ID, admin, origin)
	require.ErrorIs(t, err, ErrNotInReview)
	_, err = f.Reject(ctx, held.ID, admin, "", origin)
	var problems Invalid
	require.ErrorAs(t, err, &problems, "a reason is required")

	cancelled, err := f.Cancel(ctx, f.user, held.ID, origin)
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)
	assert.NotNil(t, cancelled.CancelledAt)
	_, err = f.Cancel(ctx, f.user, held.ID, origin)
	require.ErrorIs(t, err, ErrNotCancellable)
	available, locked := f.balance()
	assert.Equal(t, "1000", available, "released")
	assert.Equal(t, "0", locked)

	other, err := f.withdraw(f.request("350", "bigger"))
	require.NoError(t, err)
	rejected, err := f.Reject(ctx, other.ID, admin, "The destination is under investigation.", origin)
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, rejected.Status)
	assert.Equal(t, "The destination is under investigation.", rejected.Reason)
	available, _ = f.balance()
	assert.Equal(t, "1000", available)

	list, err := f.List(ctx, f.user, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, other.ID, list[0].ID, "newest first")
}

func TestWithdrawals_AreSealed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	w, err := f.withdraw(f.request("100", "sealed"))
	require.NoError(t, err)

	// Raised in SQL, so that more is paid than was locked.
	_, err = f.DB.Exec(`UPDATE withdrawals SET amount = 500 WHERE id = $1`, w.ID)
	require.NoError(t, err)
	_, err = f.Get(ctx, f.user, w.ID)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	list, err := f.List(ctx, f.user, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, list, "left out of the user's list")
	_, err = f.Cancel(ctx, f.user, w.ID, origin)
	require.ErrorIs(t, err, rowseal.ErrUnverified, "its funds stay locked until an administrator looks")
	_, locked := f.balance()
	assert.Equal(t, "101", locked)
}

// A fee that changed between the confirmation and step two is not charged
// unconfirmed (#408): the request is refused, and nothing is locked.
func TestRequest_RefusesAFeeThatChangedAfterTheConfirmation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	r := f.request("100", "key-fee")

	out, err := f.Request(ctx, f.user, r, "client", origin)
	require.NoError(t, err)
	require.NotNil(t, out.Challenge)
	assert.Equal(t, "1 USDT", out.Challenge.Details.Fee, "the user is told the fee")
	require.NoError(t, f.confirm.Confirm(ctx, f.user, out.Challenge.ID, confirm.Proof{Code: "123456"}, origin))

	_, err = f.DB.ExecContext(ctx, `UPDATE currency_networks SET withdrawal_fee = 5 WHERE currency_hk = $1 AND network = 'tron'`, f.usdt)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.CurrencyNetworks, "currency_hk = $1 AND network = 'tron'", f.usdt))

	r.ConfirmationID = out.Challenge.ID
	_, err = f.Request(ctx, f.user, r, "client", origin)
	require.ErrorIs(t, err, confirm.ErrMismatch)
	available, locked := f.balance()
	assert.Equal(t, "1000", available, "nothing is locked")
	assert.Equal(t, "0", locked)

	// The user confirms again, and the new fee is what they confirm.
	r.ConfirmationID, r.IdempotencyKey = "", "key-fee-2"
	w, err := f.withdraw(r)
	require.NoError(t, err)
	assert.Equal(t, "5", w.Fee.String())
}
