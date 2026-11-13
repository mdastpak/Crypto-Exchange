package verification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture is a store on an empty test database, with the ledger that
// withdrawal requests lock their funds in.
type fixture struct {
	Store
	books ledger.Ledger
	t     *testing.T
}

func newFixture(t *testing.T, limits map[string]map[int]config.WithdrawalLimit) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_verification")
	conn.SetMaxOpenConns(30)
	policy, err := NewPolicy(config.VerificationConfig{WithdrawalLimits: limits})
	require.NoError(t, err)
	return fixture{Store: Store{DB: conn, Policy: policy}, books: ledger.Ledger{DB: conn}, t: t}
}

// user creates an active user with a KYC tier and status, sealed when a
// sealer is active.
func (f fixture) user(tier shared.KYCTier, status shared.KYCStatus) string {
	f.t.Helper()
	var hk string
	require.NoError(f.t, f.DB.QueryRow(`INSERT INTO users (status, kyc_tier, kyc_status) VALUES ('ACTIVE', $1, $2) RETURNING hk::text`,
		tier, status).Scan(&hk))
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.Users, "hk = $1", hk))
	return hk
}

func (f fixture) currency(symbol string, fiat bool) string {
	f.t.Helper()
	var hk string
	require.NoError(f.t, f.DB.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', $1::text, 'en_name', $1::text, 'fa_name', $1::text,
			'if_fiat', $2::boolean, 'is_token', false, 'contract', '')) RETURNING hk::text`, symbol, fiat).Scan(&hk))
	return hk
}

func (f fixture) account(user string) ledger.Account {
	f.t.Helper()
	id, err := shared.UUIDFromString(user)
	require.NoError(f.t, err)
	return ledger.Account{UserHK: id}
}

func (f fixture) amount(currency, value string) ledger.Amount {
	f.t.Helper()
	id, err := shared.UUIDFromString(currency)
	require.NoError(f.t, err)
	return ledger.Amount{Currency: id, Value: dec(value)}
}

// fund credits a deposit of value to a.
func (f fixture) fund(a ledger.Account, currency, value string) {
	f.t.Helper()
	ctx := context.Background()
	key := "fund:" + shared.NewUUID().String()
	_, err := f.books.DetectDeposit(ctx, key+":detect", a, f.amount(currency, value), ledger.Ref{})
	require.NoError(f.t, err)
	_, err = f.books.CreditDeposit(ctx, key+":credit", a, f.amount(currency, value), ledger.Ref{})
	require.NoError(f.t, err)
}

// request is a withdrawal request: the check of the level and the limits,
// and the lock of the funds, in one transaction. It returns the reference
// of the withdrawal.
func (f fixture) request(user string, a ledger.Account, currency, value string) (ledger.Ref, error) {
	ctx := context.Background()
	ref := ledger.Ref{Type: ledger.RefWithdrawal, ID: shared.NewUUID().String()}
	tx, err := f.DB.BeginTx(ctx, nil)
	if err != nil {
		return ref, err
	}
	defer tx.Rollback()
	if err := f.CheckWithdrawal(ctx, tx, user, currency, dec(value)); err != nil {
		return ref, err
	}
	if _, err := f.books.In(tx).Lock(ctx, "withdrawal:"+ref.ID+":lock", a, f.amount(currency, value), ref); err != nil {
		return ref, err
	}
	return ref, tx.Commit()
}

func refusalOf(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	require.ErrorAs(t, err, &r)
	return r
}

func TestStore_LevelReadsTheSealedUserRow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	sealtest.Use(t, f.DB, rowseal.ModeEnforce)
	alice := f.user(shared.KYCTierBasic, shared.KYCStatusApproved)

	st, err := f.Level(ctx, f.DB, alice)
	require.NoError(t, err)
	assert.Equal(t, Status{Level: Level1, Tier: shared.KYCTierBasic, Status: shared.KYCStatusApproved}, st)
	require.NoError(t, f.Check(ctx, f.DB, alice, TradeOTC))
	r := refusalOf(t, f.Check(ctx, f.DB, alice, WithdrawCrypto))
	assert.Equal(t, Level2, r.Required)

	// A tier raised in SQL is not trusted: the user counts as level 0, and
	// the finding is recorded.
	_, err = f.DB.Exec(`UPDATE users SET kyc_tier = 'STANDARD' WHERE hk = $1`, alice)
	require.NoError(t, err)
	st, err = f.Level(ctx, f.DB, alice)
	require.NoError(t, err)
	assert.Equal(t, Level0, st.Level)
	r = refusalOf(t, f.Check(ctx, f.DB, alice, TradeOTC))
	assert.Equal(t, CodeVerificationRequired, r.Code)
	var findings int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'users' AND row_key = $1`, alice).Scan(&findings))
	assert.Equal(t, 1, findings)

	_, err = f.Level(ctx, f.DB, shared.NewUUID().String())
	require.ErrorIs(t, err, ErrUserNotFound)
}

// TestStore_CheckWithdrawal_CountsTheLast24Hours follows one user's rial
// withdrawals through a day: the limit per transaction, the daily limit
// across the user's accounts, a cancelled request that gives its amount
// back, and requests that do not count.
func TestStore_CheckWithdrawal_CountsTheLast24Hours(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, map[string]map[int]config.WithdrawalLimit{
		"IRR": {1: {Daily: "1000", PerTransaction: "600"}},
	})
	irr, usdt := f.currency("IRR", true), f.currency("USDT", false)
	alice := f.user(shared.KYCTierBasic, shared.KYCStatusApproved)
	main := f.account(alice)
	f.fund(main, irr, "10000")
	f.fund(main, usdt, "100")

	r := refusalOf(t, func() error { _, err := f.request(alice, main, irr, "601"); return err }())
	assert.Equal(t, PeriodTransaction, r.Period, "one request is bounded first")

	first, err := f.request(alice, main, irr, "600")
	require.NoError(t, err)

	// A sub-account of the same user shares the limit.
	var trading shared.UUID
	require.NoError(t, f.DB.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, 'trading') RETURNING id`, alice).Scan(&trading))
	sub := ledger.Account{UserHK: main.UserHK, SubAccountID: &trading}
	f.fund(sub, irr, "1000")
	r = refusalOf(t, func() error { _, err := f.request(alice, sub, irr, "500"); return err }())
	assert.Equal(t, PeriodDaily, r.Period)
	assert.Equal(t, "600", r.Used.String())
	assert.Equal(t, "400", r.Remaining().String())
	_, err = f.request(alice, sub, irr, "400")
	require.NoError(t, err, "up to the daily limit exactly")
	r = refusalOf(t, func() error { _, err := f.request(alice, main, irr, "1"); return err }())
	assert.Equal(t, CodeWithdrawalLimitExceeded, r.Code, "nothing remains")

	// A cancelled request gives its amount back.
	_, err = f.books.Release(ctx, "withdrawal:"+first.ID+":release", main, f.amount(irr, "600"), first)
	require.NoError(t, err)
	_, err = f.request(alice, main, irr, "600")
	require.NoError(t, err)

	// A lock for an order is not a withdrawal, and crypto at level 1 is
	// refused for the level, not counted.
	_, err = f.books.Lock(ctx, "order:1:lock", main, f.amount(irr, "500"), ledger.Ref{Type: "order", ID: "1"})
	require.NoError(t, err)
	used, err := withdrawn(ctx, f.DB, alice, irr, false)
	require.NoError(t, err)
	assert.Equal(t, "1000", used.String())
	r = refusalOf(t, func() error { _, err := f.request(alice, main, usdt, "1"); return err }())
	assert.Equal(t, CodeVerificationRequired, r.Code)

	// A request older than the window no longer counts.
	_, err = f.DB.Exec(`DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE balance_changes SET "timestamp" = "timestamp" - interval '25 hours'
			WHERE entry_id IN (SELECT id FROM ledger_entries WHERE reference_type = 'withdrawal');
	END $$`)
	require.NoError(t, err)
	used, err = withdrawn(ctx, f.DB, alice, irr, false)
	require.NoError(t, err)
	assert.Equal(t, "0", used.String())
	_, err = f.request(alice, main, irr, "600")
	require.NoError(t, err)
}

// TestStore_CheckWithdrawal_ConcurrentRequestsNeverPassTheLimit sends many
// requests of one user at once: the lock of the check makes each count the
// ones before it, so exactly what the limit allows passes.
func TestStore_CheckWithdrawal_ConcurrentRequestsNeverPassTheLimit(t *testing.T) {
	f := newFixture(t, map[string]map[int]config.WithdrawalLimit{"IRR": {1: {Daily: "1000"}}})
	irr := f.currency("IRR", true)
	alice := f.user(shared.KYCTierBasic, shared.KYCStatusApproved)
	main := f.account(alice)
	f.fund(main, irr, "100000")

	const requests = 25
	var passed, refused atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.request(alice, main, irr, "100")
			var r *Refusal
			switch {
			case err == nil:
				passed.Add(1)
			case errors.As(err, &r) && r.Code == CodeWithdrawalLimitExceeded:
				refused.Add(1)
			default:
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(10), passed.Load())
	assert.Equal(t, int32(requests-10), refused.Load())
	used, err := withdrawn(context.Background(), f.DB, alice, irr, false)
	require.NoError(t, err)
	assert.Equal(t, "1000", used.String())
}

func TestStore_Summary(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, map[string]map[int]config.WithdrawalLimit{
		"IRR":  {1: {Daily: "1000"}, 2: {Daily: "5000", PerTransaction: "2000"}},
		"USDT": {2: {Daily: "50"}},
		"GONE": {1: {Daily: "1"}},
	})
	irr := f.currency("IRR", true)
	usdt := f.currency("USDT", false)

	basic := f.user(shared.KYCTierBasic, shared.KYCStatusApproved)
	f.fund(f.account(basic), irr, "1000")
	_, err := f.request(basic, f.account(basic), irr, "300")
	require.NoError(t, err)
	sum, err := f.Summary(ctx, basic)
	require.NoError(t, err)
	assert.Equal(t, Level1, sum.Level)
	require.Len(t, sum.Actions, len(Actions))
	for _, a := range sum.Actions {
		assert.Equal(t, a.Action != WithdrawCrypto, a.Allowed, a.Action)
	}
	require.Len(t, sum.WithdrawalLimits, 1, "crypto is not withdrawn at level 1, and a currency that does not exist is left out")
	l := sum.WithdrawalLimits[0]
	assert.Equal(t, irr, l.CurrencyHK)
	assert.Equal(t, "IRR", l.Symbol)
	assert.Equal(t, "1000", l.Daily.String())
	assert.Nil(t, l.PerTransaction)
	assert.Equal(t, "300", l.Used.String())
	assert.Equal(t, "700", l.Remaining.String())
	assert.Equal(t, 24, l.WindowHours)

	full := f.user(shared.KYCTierStandard, shared.KYCStatusApproved)
	sum, err = f.Summary(ctx, full)
	require.NoError(t, err)
	assert.Equal(t, Level2, sum.Level)
	require.Len(t, sum.WithdrawalLimits, 2)
	assert.Equal(t, "2000", sum.WithdrawalLimits[0].PerTransaction.String())
	assert.Equal(t, usdt, sum.WithdrawalLimits[1].CurrencyHK)

	none := f.user(shared.KYCTierNone, shared.KYCStatusPending)
	sum, err = f.Summary(ctx, none)
	require.NoError(t, err)
	assert.Equal(t, Level0, sum.Level)
	assert.Empty(t, sum.WithdrawalLimits)
}

// TestStore_CheckWithdrawal_RefusesWhatItCannotCheck covers the inputs that
// are not a withdrawal it can decide.
func TestStore_CheckWithdrawal_RefusesWhatItCannotCheck(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	irr := f.currency("IRR", true)
	alice := f.user(shared.KYCTierBasic, shared.KYCStatusApproved)
	check := func(user, currency, value string) error {
		tx, err := f.DB.BeginTx(ctx, &sql.TxOptions{})
		require.NoError(t, err)
		defer tx.Rollback()
		return f.CheckWithdrawal(ctx, tx, user, currency, dec(value))
	}
	require.ErrorIs(t, check(alice, shared.NewUUID().String(), "1"), ErrUnknownCurrency)
	require.ErrorIs(t, check(shared.NewUUID().String(), irr, "1"), ErrUserNotFound)
	require.Error(t, check(alice, irr, "0"))
	require.NoError(t, check(alice, irr, fmt.Sprint(1_000_000_000_000)), "no limits without settings")
}
