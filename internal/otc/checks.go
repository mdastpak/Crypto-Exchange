package otc

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/shopspring/decimal"
)

// DBChecks checks a quote in the database, outside a transaction: the
// quote only pre-checks, and the execution (#62) checks again, under its
// locks, when the user takes it.
type DBChecks struct {
	DB           *sql.DB
	Verification verification.Store
}

// Check refuses the quote when the user's level does not allow OTC trades,
// a restriction or the security hold blocks the side, a currency does not
// allow it, or the balance of the currency the user gives is under need.
// A buy gives the quote currency and receives the base currency; a sell
// the other way round.
func (d DBChecks) Check(ctx context.Context, a Account, m currencies.PublicMarket, side Side, need decimal.Decimal) error {
	give := m.QuoteHK
	// The base currency is bought or sold; the quote currency the other
	// way round.
	baseOp, quoteOp := restrictions.OpBuy, restrictions.OpSell
	baseCap, quoteCap := currencies.CapabilityBuy, currencies.CapabilitySell
	if side == SideSell {
		give = m.BaseHK
		baseOp, quoteOp = quoteOp, baseOp
		baseCap, quoteCap = quoteCap, baseCap
	}
	// The checks run in order on one connection of the pool (#455): a
	// quote holds one connection, never one per check, so that quotes at
	// once do not starve the sign-ins, the withdrawals, and the
	// schedulers. The refusal is that of the first check that refuses, and
	// the checks after it do not run.
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("otc: a connection for the checks: %w", err)
	}
	defer conn.Close()
	for _, check := range []func() error{
		func() error { return d.Verification.Check(ctx, conn, a.UserHK, verification.TradeOTC) },
		func() error { return restrictions.Check(ctx, conn, a.UserHK, baseOp, m.BaseHK) },
		func() error { return restrictions.Check(ctx, conn, a.UserHK, quoteOp, m.QuoteHK) },
		func() error { return currencies.Check(ctx, conn, m.BaseHK, baseCap, false) },
		func() error { return currencies.Check(ctx, conn, m.QuoteHK, quoteCap, false) },
		func() error { return d.balance(ctx, conn, a, give, need) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (d DBChecks) balance(ctx context.Context, q ledger.RowQuerier, a Account, currencyHK string, need decimal.Decimal) error {
	user, err := shared.UUIDFromString(a.UserHK)
	if err != nil {
		return fmt.Errorf("otc: the user %q: %w", a.UserHK, err)
	}
	account := ledger.Account{UserHK: user}
	if a.SubAccountID != nil {
		sub, err := shared.UUIDFromString(*a.SubAccountID)
		if err != nil {
			return &Refusal{Code: CodeInvalidQuoteRequest, Limit: "sub_account_id"}
		}
		account.SubAccountID = &sub
	}
	currency, err := shared.UUIDFromString(currencyHK)
	if err != nil {
		return fmt.Errorf("otc: the currency %q: %w", currencyHK, err)
	}
	b, err := ledger.BalanceIn(ctx, q, account, currency)
	if err != nil {
		return err
	}
	if b.Available.Decimal().LessThan(need) {
		return ErrInsufficientBalance
	}
	return nil
}
