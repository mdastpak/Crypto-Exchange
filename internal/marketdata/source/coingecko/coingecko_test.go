package coingecko

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/sourcetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureServer answers like the CoinGecko API from testdata. The fixtures
// follow the documented answers of /coins/markets and /coins/{id}.
func fixtureServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		assert.Equal(t, "demo-key", r.Header.Get("x-cg-demo-api-key"))
		switch {
		case r.URL.Path == "/api/v3/coins/markets":
			q := r.URL.Query()
			assert.Equal(t, "usd", q.Get("vs_currency"))
			assert.Equal(t, "market_cap_desc", q.Get("order"))
			if q.Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			body, err := os.ReadFile("testdata/markets_page1.json")
			require.NoError(t, err)
			if ids := q.Get("ids"); ids != "" {
				// Keep the rows of the asked ids only.
				var kept []string
				for _, row := range strings.Split(strings.Trim(strings.TrimSpace(string(body)), "[]"), "},") {
					for _, id := range strings.Split(ids, ",") {
						if strings.Contains(row, `"id": "`+id+`"`) {
							kept = append(kept, strings.TrimSuffix(strings.TrimSpace(row), "}")+"}")
						}
					}
				}
				body = []byte("[" + strings.Join(kept, ",") + "]")
			}
			_, _ = w.Write(body)
		case strings.HasPrefix(r.URL.Path, "/api/v3/coins/"):
			assert.Equal(t, "false", r.URL.Query().Get("market_data"))
			body, err := os.ReadFile("testdata/coin_" + strings.TrimPrefix(r.URL.Path, "/api/v3/coins/") + ".json")
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"coin not found"}`))
				return
			}
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func newSource(t *testing.T) (*Source, *[]string) {
	t.Helper()
	srv, requests := fixtureServer(t)
	client, err := source.NewClient(source.ClientOptions{Source: Name, BaseURL: srv.URL + "/api/v3", Timeout: 5 * time.Second,
		Header: http.Header{"x-cg-demo-api-key": {"demo-key"}}})
	require.NoError(t, err)
	return New(client), requests
}

func TestSource_KeepsTheCatalogContract(t *testing.T) {
	src, _ := newSource(t)
	sourcetest.CheckCatalog(t, src)
}

func TestAssets_MapsTheListing(t *testing.T) {
	src, requests := newSource(t)
	assets, err := src.Assets(context.Background(), source.ListOptions{})
	require.NoError(t, err)
	require.Len(t, assets, 3, "a row without an id is left out")
	assert.Len(t, *requests, 1, "a short page is the last one")

	btc := assets[0]
	assert.Equal(t, "bitcoin", btc.ExternalID)
	assert.Equal(t, "BTC", btc.Details.Symbol)
	assert.Equal(t, 1, *btc.Details.MarketCapRank)
	assert.Equal(t, "21000000", btc.Details.MaxSupply.String())
	assert.Equal(t, Attribution, btc.Details.Attribution)

	eth := assets[1]
	assert.Equal(t, "120300000.123456789012345678", eth.Details.CirculatingSupply.String(), "every digit stays")
	assert.Nil(t, eth.Details.MaxSupply, "a null supply stays missing")

	assert.Equal(t, "", assets[2].Details.LogoURL, "an http logo is dropped")
}

func TestAssets_ListsIDs(t *testing.T) {
	src, requests := newSource(t)
	assets, err := src.Assets(context.Background(), source.ListOptions{IDs: []string{"tether", "bitcoin"}})
	require.NoError(t, err)
	require.Len(t, assets, 2)
	assert.Contains(t, (*requests)[0], "ids=tether%2Cbitcoin")
}

func TestAsset_ReadsTheRecordSafely(t *testing.T) {
	src, _ := newSource(t)
	usdt, err := src.Asset(context.Background(), "tether")
	require.NoError(t, err)
	assert.Equal(t, marketdata.KindToken, usdt.Details.Kind)
	assert.Equal(t, []source.Contract{
		{Network: "ethereum", Address: "0xdac17f958d2ee523a2206206994597c13d831ec7"},
		{Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
	}, usdt.Contracts)

	d := usdt.Details.Description
	assert.NotContains(t, d, "<")
	assert.NotContains(t, d, "alert", "a script's text goes with its tags")
	assert.Contains(t, d, `pegged to the "US dollar" & backed.`)
	assert.Contains(t, d, "Tether (USDT) is a stablecoin.\n\n")

	assert.Equal(t, "https://tether.to/", usdt.Details.Links["website"], "the first https homepage")
	assert.Equal(t, "https://x.com/Tether_to", usdt.Details.Links["twitter"])
	assert.NotContains(t, usdt.Details.Links, "reddit", "an http link is dropped")
	assert.Equal(t, "https://coin-images.coingecko.com/coins/images/325/large/Tether.png", usdt.Details.LogoURL)

	btc, err := src.Asset(context.Background(), "bitcoin")
	require.NoError(t, err)
	assert.Equal(t, marketdata.KindCoin, btc.Details.Kind)
	assert.Empty(t, btc.Contracts)
	assert.NotContains(t, btc.Details.Links, "website", "http://www.bitcoin.org is not https")
	assert.NotContains(t, btc.Details.Links, "twitter", "an invalid name is dropped")
	assert.Equal(t, "https://github.com/bitcoin/bitcoin", btc.Details.Links["source_code"])

	_, err = src.Asset(context.Background(), "../admin")
	assert.True(t, errors.Is(err, source.ErrNotFound))
}

func TestSettings_PicksTheKeyHeaderOfThePlan(t *testing.T) {
	assert.Equal(t, "x-cg-demo-api-key", Settings("").KeyHeader)
	assert.Equal(t, "x-cg-pro-api-key", Settings("https://pro-api.coingecko.com/api/v3").KeyHeader)
}
