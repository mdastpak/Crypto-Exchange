package nobitex

import (
	"context"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/venuetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newConnector(t *testing.T, token string) (*Nobitex, *fake) {
	f, srv := newFake(t)
	client, err := source.NewClient(source.ClientOptions{Source: "venue_nobitex", BaseURL: srv.URL, Timeout: 2 * time.Second, MaxAttempts: 2})
	require.NoError(t, err)
	if token == "" {
		token = f.token
	}
	n, err := New(Options{Client: client, Credentials: venues.NewCached(venues.Static{"nobitex": {Token: venues.NewSecret(token)}}, time.Minute)})
	require.NoError(t, err)
	return n, f
}

func TestNobitex_PassesTheContract(t *testing.T) {
	venuetest.Run(t, func(t *testing.T) venuetest.Harness {
		n, f := newConnector(t, "")
		return venuetest.Harness{Venue: n, Symbol: "btc-usdt", Quantity: shared.MustNewDecimalFromString("0.01"),
			LimitPrice: shared.MustNewDecimalFromString("50000"), Secrets: []string{f.token},
			LoseNextAnswer: func() { f.mu.Lock(); f.loseNext = true; f.mu.Unlock() },
			Unsupported: []venues.Capability{venues.CapabilityWithdraw, venues.CapabilityNetworks,
				venues.CapabilityInternalTransfer, venues.CapabilityWhitelist}}
	})
}

func TestNobitex_ARefusalOnHTTP200IsARejection(t *testing.T) {
	n, _ := newConnector(t, "")
	_, err := n.PlaceOrder(context.Background(), venues.OrderRequest{ClientID: "c1", Symbol: "eth-usdt", Side: venues.SideBuy,
		Type: venues.OrderMarket, Quantity: shared.MustNewDecimalFromString("1")})
	var ve *venues.Error
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, venues.KindRejected, ve.Kind)
	assert.Equal(t, "InvalidMarketPair", ve.Code)
}

func TestNobitex_ARefusedTokenIsNotInTheError(t *testing.T) {
	n, _ := newConnector(t, "a-wrong-token-DDDD")
	_, err := n.Balances(context.Background())
	require.Error(t, err)
	assert.Equal(t, venues.KindRejected, venues.KindOf(err))
	assert.NotContains(t, err.Error(), "a-wrong-token-DDDD")
}

func TestNobitex_ListsTheMarketsWithoutFilters(t *testing.T) {
	n, _ := newConnector(t, "")
	instruments, err := n.Instruments(context.Background())
	require.NoError(t, err)
	require.Len(t, instruments, 3)
	bySymbol := map[string]venues.Instrument{}
	for _, i := range instruments {
		bySymbol[i.Symbol] = i
	}
	assert.Equal(t, "rls", bySymbol["btc-rls"].Quote, "rial markets stay in rials")
	assert.False(t, bySymbol["shib-rls"].Trading)
	assert.True(t, bySymbol["btc-usdt"].LotSize.IsZero(), "Nobitex publishes no steps")
}

func TestNobitex_ReadsTheWalletsAndADepositAddress(t *testing.T) {
	n, _ := newConnector(t, "")
	balances, err := n.Balances(context.Background())
	require.NoError(t, err)
	assert.Len(t, balances, 2)
	addr, err := n.DepositAddress(context.Background(), "usdt", "TRX")
	require.NoError(t, err)
	assert.Equal(t, "TNobitexDepositOfOurs", addr.Address)
	assert.True(t, n.Supports(venues.CapabilityDepositAddress))
	assert.Equal(t, venues.ModeLive, n.Mode())
}

// TestNobitex_ReadsTheBestPrices covers the pre-check of #515: bestBuy and
// bestSell of /market/stats.
func TestNobitex_ReadsTheBestPrices(t *testing.T) {
	n, _ := newConnector(t, "")
	book, err := n.BestPrices(context.Background(), "btc-usdt")
	require.NoError(t, err)
	assert.Equal(t, "60000", book.Bid.String())
	assert.Equal(t, "60010", book.Ask.String())
	_, err = n.BestPrices(context.Background(), "eth-usdt")
	require.ErrorIs(t, err, venues.ErrNotFound)
	closed, err := n.BestPrices(context.Background(), "shib-rls")
	require.NoError(t, err)
	assert.True(t, closed.Bid.IsZero() && closed.Ask.IsZero(), "a closed market without prices has an empty book")
}
