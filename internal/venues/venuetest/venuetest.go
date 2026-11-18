// Package venuetest is the contract that every venue connector passes
// (#384): the same scenarios run against the stub and against each
// connector, which a fake server of the venue's API serves from
// hand-written fixtures. A connector that passes it can be relied on by
// the router (#386) for what matters after a crash: an order found by our
// id, an answer lost after the venue did it told apart from a refusal, and
// no credential in what it returns.
package venuetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/venues"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Harness is one venue under test, fresh for each scenario.
type Harness struct {
	Venue venues.Venue
	// Symbol is a market that fills a market order of Quantity at once, and
	// keeps a limit order at LimitPrice open.
	Symbol     string
	Quantity   shared.Decimal
	LimitPrice shared.Decimal
	// LoseNextAnswer makes the venue do the next call that changes
	// something and lose its answer, as a timeout after it was sent would.
	LoseNextAnswer func()
	// Secrets are the credentials that must not be in any error.
	Secrets []string
	// Unsupported are capabilities that the venue does not offer, whose
	// calls must answer ErrUnsupported.
	Unsupported []venues.Capability
}

// Run runs the contract on the harness that newHarness makes.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()
	ctx := context.Background()
	var errs []error
	keep := func(err error) error {
		if err != nil {
			errs = append(errs, err)
		}
		return err
	}

	t.Run("an order it does not have is not found", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.Venue.GetOrder(ctx, h.Symbol, "never-sent-1")
		_ = keep(err)
		assert.ErrorIs(t, err, venues.ErrNotFound)
	})

	t.Run("a market order is found by our id", func(t *testing.T) {
		h := newHarness(t)
		placed, err := h.Venue.PlaceOrder(ctx, venues.OrderRequest{ClientID: "contract-market-1", Symbol: h.Symbol,
			Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: h.Quantity})
		require.NoError(t, keep(err))
		assert.Equal(t, "contract-market-1", placed.ClientID)
		assert.NotEmpty(t, placed.VenueID)
		got, err := h.Venue.GetOrder(ctx, h.Symbol, "contract-market-1")
		require.NoError(t, keep(err))
		assert.Equal(t, placed.VenueID, got.VenueID)
		assert.Equal(t, venues.OrderFilled, got.Status)
		assert.True(t, got.Filled.Equal(h.Quantity), "filled %s of %s", got.Filled, h.Quantity)
		assert.True(t, got.FilledQuote.IsPositive())
	})

	t.Run("an open order with our id again is a duplicate", func(t *testing.T) {
		h := newHarness(t)
		req := venues.OrderRequest{ClientID: "contract-limit-1", Symbol: h.Symbol, Side: venues.SideBuy, Type: venues.OrderLimit,
			Quantity: h.Quantity, Price: h.LimitPrice}
		first, err := h.Venue.PlaceOrder(ctx, req)
		require.NoError(t, keep(err))
		assert.False(t, first.Status.Done())
		_, err = h.Venue.PlaceOrder(ctx, req)
		_ = keep(err)
		var ve *venues.Error
		require.ErrorAs(t, err, &ve)
		assert.Equal(t, venues.KindRejected, ve.Kind)
		assert.Equal(t, venues.CodeDuplicate, ve.Code)
	})

	t.Run("a lost answer is an unknown outcome, and the order is found after it", func(t *testing.T) {
		h := newHarness(t)
		h.LoseNextAnswer()
		_, err := h.Venue.PlaceOrder(ctx, venues.OrderRequest{ClientID: "contract-lost-1", Symbol: h.Symbol,
			Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: h.Quantity})
		_ = keep(err)
		require.Error(t, err)
		assert.Equal(t, venues.KindUnknown, venues.KindOf(err), "%v", err)
		// As after a crash: look it up by our id before anything else.
		got, err := h.Venue.GetOrder(ctx, h.Symbol, "contract-lost-1")
		require.NoError(t, keep(err))
		assert.Equal(t, venues.OrderFilled, got.Status)
	})

	t.Run("a limit order is cancelled", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.Venue.PlaceOrder(ctx, venues.OrderRequest{ClientID: "contract-cancel-1", Symbol: h.Symbol,
			Side: venues.SideBuy, Type: venues.OrderLimit, Quantity: h.Quantity, Price: h.LimitPrice})
		require.NoError(t, keep(err))
		cancelled, err := h.Venue.CancelOrder(ctx, h.Symbol, "contract-cancel-1")
		require.NoError(t, keep(err))
		assert.Equal(t, venues.OrderCancelled, cancelled.Status)
		assert.Equal(t, "contract-cancel-1", cancelled.ClientID)
		got, err := h.Venue.GetOrder(ctx, h.Symbol, "contract-cancel-1")
		require.NoError(t, keep(err))
		assert.Equal(t, venues.OrderCancelled, got.Status)
	})

	t.Run("an order beyond the funds is rejected", func(t *testing.T) {
		h := newHarness(t)
		huge := h.Quantity.Mul(shared.NewDecimalFromInt64(1_000_000_000))
		_, err := h.Venue.PlaceOrder(ctx, venues.OrderRequest{ClientID: "contract-funds-1", Symbol: h.Symbol,
			Side: venues.SideBuy, Type: venues.OrderMarket, Quantity: huge})
		_ = keep(err)
		require.Error(t, err)
		assert.Equal(t, venues.KindRejected, venues.KindOf(err), "%v", err)
		_, err = h.Venue.GetOrder(ctx, h.Symbol, "contract-funds-1")
		assert.ErrorIs(t, keep(err), venues.ErrNotFound)
	})

	t.Run("balances and markets are read", func(t *testing.T) {
		h := newHarness(t)
		balances, err := h.Venue.Balances(ctx)
		require.NoError(t, keep(err))
		assert.NotEmpty(t, balances)
		instruments, err := h.Venue.Instruments(ctx)
		require.NoError(t, keep(err))
		found := false
		for _, i := range instruments {
			found = found || i.Symbol == h.Symbol
		}
		assert.True(t, found, "the market %s is listed", h.Symbol)
	})

	t.Run("what it does not offer is unsupported", func(t *testing.T) {
		h := newHarness(t)
		for _, c := range h.Unsupported {
			assert.False(t, h.Venue.Supports(c), c)
			err := callCapability(ctx, h.Venue, c)
			_ = keep(err)
			assert.ErrorIs(t, err, venues.ErrUnsupported, c)
		}
	})

	t.Run("no error holds a credential", func(t *testing.T) {
		h := newHarness(t)
		for _, err := range errs {
			text := fmt.Sprintf("%v %+v %#v", err, err, err)
			for _, s := range h.Secrets {
				assert.NotContains(t, text, s)
			}
			assert.NotContains(t, text, "signature=")
		}
	})
}

// callCapability calls what c offers.
func callCapability(ctx context.Context, v venues.Venue, c venues.Capability) error {
	one := shared.NewDecimalFromInt64(1)
	switch c {
	case venues.CapabilityWithdraw:
		_, err := v.Withdraw(ctx, venues.WithdrawRequest{ClientID: "contract-w-1", Asset: "USDT", Network: "TRX", Address: "T1", Amount: one})
		if _, err := v.GetWithdrawal(ctx, "contract-w-1"); !errors.Is(err, venues.ErrUnsupported) {
			return fmt.Errorf("GetWithdrawal: %w", err)
		}
		return err
	case venues.CapabilityDepositAddress:
		_, err := v.DepositAddress(ctx, "USDT", "TRX")
		return err
	case venues.CapabilityNetworks:
		_, err := v.Networks(ctx, "USDT")
		return err
	case venues.CapabilityInternalTransfer:
		_, err := v.InternalTransfer(ctx, venues.TransferRequest{ClientID: "contract-t-1", Asset: "USDT", Amount: one, To: "sub"})
		if _, err := v.GetTransfer(ctx, "contract-t-1"); !errors.Is(err, venues.ErrUnsupported) {
			return fmt.Errorf("GetTransfer: %w", err)
		}
		return err
	case venues.CapabilityWhitelist:
		_, err := v.Whitelisted(ctx, "USDT", "TRX", "T1", "")
		return err
	}
	return fmt.Errorf("unknown capability %s", c)
}

// HasNo reports whether text has none of secrets.
func HasNo(text string, secrets ...string) bool {
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			return false
		}
	}
	return true
}
