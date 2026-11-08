// Package sourcetest checks that a source keeps the contract of its
// interface, so that every source, real or stub, passes the same checks.
// Only tests import it.
package sourcetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CheckCatalog checks a catalog source: the listing honors its limit, and
// every asset has an id, an upper-case symbol, a name, https links, and no
// negative supply; a full asset matches its listing; an unknown id is
// ErrNotFound.
func CheckCatalog(t *testing.T, src source.CatalogSource) {
	t.Helper()
	ctx := context.Background()
	require.NotEmpty(t, src.Name())

	listed, err := src.Assets(ctx, source.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(listed), 2, "the listing honors its limit")
	all, err := src.Assets(ctx, source.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, all)
	for _, a := range all {
		checkAsset(t, a)
	}

	full, err := src.Asset(ctx, all[0].ExternalID)
	require.NoError(t, err)
	checkAsset(t, full)
	assert.Equal(t, all[0].ExternalID, full.ExternalID)
	assert.Equal(t, all[0].Details.Symbol, full.Details.Symbol)

	only, err := src.Assets(ctx, source.ListOptions{IDs: []string{all[0].ExternalID}})
	require.NoError(t, err)
	require.Len(t, only, 1, "a listing of ids returns those ids")
	assert.Equal(t, all[0].ExternalID, only[0].ExternalID)

	_, err = src.Asset(ctx, "no-such-asset-anywhere")
	assert.True(t, errors.Is(err, source.ErrNotFound), "an unknown id is ErrNotFound: %v", err)
}

func checkAsset(t *testing.T, a source.Asset) {
	t.Helper()
	d := a.Details
	assert.NotEmpty(t, a.ExternalID)
	assert.NotEmpty(t, a.Slug, a.ExternalID)
	assert.Equal(t, strings.ToUpper(d.Symbol), d.Symbol, a.ExternalID)
	assert.NotEmpty(t, d.Name, a.ExternalID)
	assert.NotEmpty(t, d.Attribution, "%s: the source of the data is attributed", a.ExternalID)
	for kind, link := range d.Links {
		assert.True(t, strings.HasPrefix(link, "https://"), "%s: the %s link is https", a.ExternalID, kind)
	}
	if d.LogoURL != "" {
		assert.True(t, strings.HasPrefix(d.LogoURL, "https://"), a.ExternalID)
	}
	for _, s := range []*shared.Decimal{d.CirculatingSupply, d.TotalSupply, d.MaxSupply} {
		if s != nil {
			assert.False(t, s.IsNegative(), a.ExternalID)
		}
	}
	for _, c := range a.Contracts {
		assert.NotEmpty(t, c.Network, a.ExternalID)
		assert.Equal(t, strings.ToLower(c.Network), c.Network, a.ExternalID)
		assert.NotEmpty(t, c.Address, a.ExternalID)
	}
}

// CheckVenue checks a venue source: the venue is valid, and every ticker
// has a symbol, upper-case base and quote, a non-negative price, a bid not
// above the ask, and a time.
func CheckVenue(t *testing.T, src source.VenueSource) {
	t.Helper()
	v := src.Venue()
	assert.Equal(t, src.Name(), v.ID)
	assert.Contains(t, []string{marketdata.VenueCEX, marketdata.VenueDEX}, v.Kind)
	assert.Contains(t, []string{marketdata.RegionGlobal, marketdata.RegionIran}, v.Region)
	assert.NotEmpty(t, v.Attribution)

	tickers, err := src.Tickers(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, tickers)
	for _, tk := range tickers {
		assert.NotEmpty(t, tk.VenueSymbol)
		assert.Equal(t, strings.ToUpper(tk.Base), tk.Base, tk.VenueSymbol)
		assert.Equal(t, strings.ToUpper(tk.Quote), tk.Quote, tk.VenueSymbol)
		assert.False(t, tk.Last.IsNegative(), tk.VenueSymbol)
		if tk.Bid != nil && tk.Ask != nil {
			assert.True(t, tk.Bid.LessThanOrEqual(*tk.Ask), "%s: the bid is not above the ask", tk.VenueSymbol)
		}
		assert.False(t, tk.ObservedAt.IsZero(), tk.VenueSymbol)
		assert.Equal(t, time.UTC, tk.ObservedAt.Location(), tk.VenueSymbol)
	}
}

// CheckCandles checks a candle source on a pair it has: candles are closed,
// in the range, oldest first, aligned to the interval, at most MaxCandles,
// with low <= open, close <= high.
func CheckCandles(t *testing.T, src source.CandleSource, venueSymbol string, interval source.Interval, now time.Time) {
	t.Helper()
	step := interval.Duration()
	to := now.Truncate(step)
	from := to.Add(-10 * step)
	candles, err := src.Candles(context.Background(), venueSymbol, interval, from, to)
	require.NoError(t, err)
	require.NotEmpty(t, candles)
	assert.LessOrEqual(t, len(candles), src.MaxCandles())
	for i, c := range candles {
		assert.False(t, c.OpenTime.Before(from), "in the range")
		assert.True(t, c.OpenTime.Add(step).Compare(now) <= 0, "closed")
		assert.True(t, c.OpenTime.Equal(c.OpenTime.Truncate(step)), "aligned")
		if i > 0 {
			assert.True(t, c.OpenTime.After(candles[i-1].OpenTime), "oldest first")
		}
		assert.True(t, c.Low.LessThanOrEqual(c.Open) && c.Low.LessThanOrEqual(c.Close))
		assert.True(t, c.High.GreaterThanOrEqual(c.Open) && c.High.GreaterThanOrEqual(c.Close))
		assert.False(t, c.Volume.IsNegative())
	}
}
