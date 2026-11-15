package withdrawals

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var admin = audit.Event{ActorType: audit.ActorAdmin, ActorID: "5f0c9b4e-2d1a-4c3b-9e8f-7a6b5c4d3e2f"}

// payouts returns the payouts of f by the manual provider, at now.
func (f fixture) payouts(settings config.PayoutsConfig, now *time.Time) Payouts {
	book, ok := f.Destinations.(destinations.Store)
	require.True(f.t, ok)
	return Payouts{DB: f.DB, Ledger: f.Ledger, Destinations: book, Provider: Manual{}, Settings: settings,
		Location: time.UTC, Now: func() time.Time { return *now }}
}

func (f fixture) payoutsOf(withdrawalID string) []Payout {
	f.t.Helper()
	list, err := payoutsOf(context.Background(), f.DB, withdrawalID)
	require.NoError(f.t, err)
	return list
}

func (f fixture) balanceOf(currency string) (available, locked string) {
	f.t.Helper()
	b, err := f.Ledger.Balance(context.Background(), account(f.user), uuidOf(currency))
	require.NoError(f.t, err)
	return b.Available.String(), b.Locked.String()
}

func TestPayouts_ACryptoWithdrawalIsPaidOnChain(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	now := time.Now()
	p := f.payouts(config.PayoutsConfig{}, &now)
	w, err := f.withdraw(f.request("100", "chain"))
	require.NoError(t, err)

	moved, err := p.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, moved, "planned and sent")
	got, err := f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusProcessing, got.Status)
	require.Len(t, got.Payouts, 1)
	pay := got.Payouts[0]
	assert.Equal(t, RailChain, pay.Rail)
	assert.Equal(t, "100", pay.Amount.String())
	assert.Equal(t, "1", pay.Fee.String(), "the whole fee")
	assert.Equal(t, PayoutSubmitted, pay.Status)
	_, err = f.Cancel(ctx, f.user, w.ID, origin)
	require.ErrorIs(t, err, ErrNotCancellable, "being paid")

	queue, err := p.List(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, f.user, queue[0].UserHK, "the administrators see whose it is")
	detail, err := p.Detail(ctx, pay.ID, admin)
	require.NoError(t, err)
	assert.Equal(t, "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8", detail.DestinationValue, "to pay it by hand")

	_, err = p.Complete(ctx, pay.ID, "", admin)
	var problems Invalid
	require.ErrorAs(t, err, &problems, "a tracking number is required")
	done, err := p.Complete(ctx, pay.ID, "0x9f2c", admin)
	require.NoError(t, err)
	assert.Equal(t, PayoutCompleted, done.Status)
	_, err = p.Complete(ctx, pay.ID, "0x9f2c", admin)
	require.ErrorIs(t, err, ErrPayoutNotOpen, "never paid twice")

	got, err = f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.NotNil(t, got.CompletedAt)
	assert.Equal(t, "0x9f2c", got.Payouts[0].TrackingID)
	available, locked := f.balanceOf(f.usdt)
	assert.Equal(t, "899", available)
	assert.Equal(t, "0", locked, "the amount and the fee left")
	var views int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1`, audit.ActionWithdrawalPayoutView).Scan(&views))
	assert.Equal(t, 1, views, "reading a destination in the clear is audited")
}

// TestPayouts_NeedOneConnectionOfThePool runs a pass, the detail, and the
// completion of a payout with a pool of one connection (#405): a step that
// held its transaction while it read on another connection would wait for
// itself.
func TestPayouts_NeedOneConnectionOfThePool(t *testing.T) {
	f := newFixture(t, "")
	now := time.Now()
	p := f.payouts(config.PayoutsConfig{}, &now)
	w, err := f.withdraw(f.request("100", "pool-payout"))
	require.NoError(t, err)

	f.DB.SetMaxOpenConns(1)
	t.Cleanup(func() { f.DB.SetMaxOpenConns(0) })
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			ctx := context.Background()
			if _, err := p.Run(ctx); err != nil {
				return err
			}
			list, err := payoutsOf(ctx, f.DB, w.ID)
			if err != nil {
				return err
			}
			if len(list) != 1 {
				return fmt.Errorf("%d payouts, want 1", len(list))
			}
			if _, err := p.Detail(ctx, list[0].ID, admin); err != nil {
				return err
			}
			_, err = p.Complete(ctx, list[0].ID, "0x9f2c", admin)
			return err
		}()
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("a step of the payouts waits for a second connection of the pool")
	}
	available, locked := f.balanceOf(f.usdt)
	assert.Equal(t, "899", available)
	assert.Equal(t, "0", locked)
}

func TestPayouts_ARialWithdrawalIsSplitAcrossRailsAndDays(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	iban, irr := f.rials(t, "5000000000")
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	p := f.payouts(config.PayoutsConfig{
		RailOrder: []string{"satna", "paya"},
		Rails: map[string]config.RailConfig{
			"satna": {Min: "1500000000", Max: "2000000000", DailyPerDestination: "2000000000"},
			"paya":  {Max: "1000000000", DailyPerDestination: "1000000000"},
		},
	}, &now)
	amount, err := shared.NewDecimalFromString("4000000000")
	require.NoError(t, err)
	w, err := f.withdraw(Request{CurrencyHK: irr, Network: "internal", DestinationID: iban, Amount: amount, IdempotencyKey: "rials"})
	require.NoError(t, err)
	assert.Equal(t, "10000", w.Fee.String())

	_, err = p.Run(ctx)
	require.NoError(t, err)
	payouts := f.payoutsOf(w.ID)
	require.Len(t, payouts, 3)
	var plan []string
	for _, pay := range payouts {
		plan = append(plan, fmt.Sprintf("%s %s+%s %s", pay.Rail, pay.Amount, pay.Fee, pay.Status))
	}
	assert.Equal(t, []string{
		"satna 2000000000+5000 submitted",
		"paya 1000000000+2500 submitted",
		"paya 1000000000+2500 planned",
	}, plan, "today's payouts are sent; tomorrow's waits")
	assert.True(t, payouts[2].ScheduledFor.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)))

	_, err = p.Complete(ctx, payouts[0].ID, "SATNA-1", admin)
	require.NoError(t, err)
	got, err := f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPartiallyPaid, got.Status)
	assert.Nil(t, got.CompletedAt)

	// PAYA fails; SATNA has nothing left today, and its minimum is above
	// the part tomorrow: the part is released alone.
	failed, err := p.Fail(ctx, payouts[1].ID, "The destination bank refused the transfer.", admin)
	require.NoError(t, err)
	assert.Equal(t, PayoutReleased, failed.Status)
	_, err = p.Fail(ctx, payouts[0].ID, "late", admin)
	require.ErrorIs(t, err, ErrPayoutNotOpen, "the paid payout stays paid")

	now = now.Add(24 * time.Hour)
	_, err = p.Run(ctx)
	require.NoError(t, err)
	payouts = f.payoutsOf(w.ID)
	require.Equal(t, PayoutSubmitted, payouts[2].Status, "tomorrow's payout is sent on its day")
	_, err = p.Complete(ctx, payouts[2].ID, "PAYA-2", admin)
	require.NoError(t, err)

	got, err = f.Get(ctx, f.user, w.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPartiallyPaid, got.Status, "settled, with a part released")
	assert.NotNil(t, got.CompletedAt)
	assert.Contains(t, got.Reason, "released")
	available, locked := f.balanceOf(irr)
	assert.Equal(t, "1999992500", available, "5000000000 less the two payouts paid and their fees")
	assert.Equal(t, "0", locked)
}

func TestPayouts_ARailOpensOnItsHours(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	iban, irr := f.rials(t, "5000000000")
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	p := f.payouts(config.PayoutsConfig{
		RailOrder: []string{"paya"},
		Rails:     map[string]config.RailConfig{"paya": {Max: "1000000000", Hours: "08:00-17:00"}},
	}, &now)
	amount, err := shared.NewDecimalFromString("900000000")
	require.NoError(t, err)
	w, err := f.withdraw(Request{CurrencyHK: irr, Network: "internal", DestinationID: iban, Amount: amount, IdempotencyKey: "evening"})
	require.NoError(t, err)

	_, err = p.Run(ctx)
	require.NoError(t, err)
	pay := f.payoutsOf(w.ID)[0]
	assert.Equal(t, PayoutPlanned, pay.Status, "PAYA is closed in the evening")
	assert.True(t, pay.ScheduledFor.Equal(time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)), "it waits for the morning")

	now = time.Date(2026, 11, 16, 8, 30, 0, 0, time.UTC)
	_, err = p.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, PayoutSubmitted, f.payoutsOf(w.ID)[0].Status)
}

func TestPayouts_AreSealed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "")
	now := time.Now()
	p := f.payouts(config.PayoutsConfig{}, &now)
	w, err := f.withdraw(f.request("100", "sealed-payout"))
	require.NoError(t, err)
	_, err = p.Run(ctx)
	require.NoError(t, err)
	pay := f.payoutsOf(w.ID)[0]

	// Raised in SQL, so that more is paid than the user withdrew.
	_, err = f.DB.Exec(`UPDATE withdrawal_payouts SET amount = 500 WHERE id = $1`, pay.ID)
	require.NoError(t, err)
	_, err = p.Complete(ctx, pay.ID, "0x1", admin)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	_, err = p.List(ctx, "", 10)
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Empty(t, f.payoutsOf(w.ID), "left out of the user's view")
	_, locked := f.balanceOf(f.usdt)
	assert.Equal(t, "101", locked, "nothing left the balance")
}

// rials gives the user a verified national ID, an IBAN in their book, and
// a balance of IRR. It returns the IBAN's destination and the currency.
func (f fixture) rials(t *testing.T, balance string) (iban, irr string) {
	t.Helper()
	ctx := context.Background()
	book, ok := f.Destinations.(destinations.Store)
	require.True(t, ok)
	require.NoError(t, identity.SaveNationalID(ctx, f.DB, book.NationalIDs, f.user, "0499370899", "stub", "", time.Now()))
	d, err := book.Add(ctx, f.user, destinations.Request{Kind: destinations.KindIBAN, Number: makeIBAN("0540102680020817909002")}, origin)
	require.NoError(t, err)
	require.NoError(t, f.DB.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'IRR'`).Scan(&irr))
	value, err := shared.NewDecimalFromString(balance)
	require.NoError(t, err)
	deposit := ledger.Amount{Currency: uuidOf(irr), Value: value}
	_, err = f.Ledger.DetectDeposit(ctx, "irr:detect", account(f.user), deposit, ledger.Ref{Type: "deposit", ID: "irr"})
	require.NoError(t, err)
	_, err = f.Ledger.CreditDeposit(ctx, "irr:credit", account(f.user), deposit, ledger.Ref{Type: "deposit", ID: "irr"})
	require.NoError(t, err)
	return d.ID, irr
}

// makeIBAN returns an Iranian IBAN of bban with its check digits.
func makeIBAN(bban string) string {
	rem := 0
	for _, c := range []byte(bban + "182700") {
		rem = (rem*10 + int(c-'0')) % 97
	}
	return fmt.Sprintf("IR%02d%s", 98-rem, bban)
}
