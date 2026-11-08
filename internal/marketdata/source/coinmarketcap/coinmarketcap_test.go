package coinmarketcap

import (
	"context"
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

// fixtureServer answers like the CoinMarketCap API from testdata, which
// follows its documented answers.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "cmc-key", r.Header.Get("X-CMC_PRO_API_KEY"))
		switch r.URL.Path {
		case "/v1/cryptocurrency/listings/latest":
			if r.URL.Query().Get("start") != "1" {
				_, _ = w.Write([]byte(`{"status":{"error_code":0},"data":[]}`))
				return
			}
			body, err := os.ReadFile("testdata/listings.json")
			require.NoError(t, err)
			_, _ = w.Write(body)
		case "/v2/cryptocurrency/info":
			body, err := os.ReadFile("testdata/info.json")
			require.NoError(t, err)
			// Keep the asked ids only, as skip_invalid does.
			asked := strings.Split(r.URL.Query().Get("id"), ",")
			if len(asked) == 1 && asked[0] != "1" && asked[0] != "825" {
				_, _ = w.Write([]byte(`{"status":{"error_code":0},"data":{}}`))
				return
			}
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSource(t *testing.T) *Source {
	t.Helper()
	client, err := source.NewClient(source.ClientOptions{Source: Name, BaseURL: fixtureServer(t).URL, Timeout: 5 * time.Second,
		Header: http.Header{Settings.KeyHeader: {"cmc-key"}}})
	require.NoError(t, err)
	return New(client)
}

func TestSource_KeepsTheCatalogContract(t *testing.T) {
	sourcetest.CheckCatalog(t, newSource(t))
}

func TestAssets_MapsTheListing(t *testing.T) {
	assets, err := newSource(t).Assets(context.Background(), source.ListOptions{})
	require.NoError(t, err)
	require.Len(t, assets, 3)
	usdt := assets[1]
	assert.Equal(t, "825", usdt.ExternalID)
	assert.Equal(t, "tether", usdt.Slug)
	assert.Equal(t, marketdata.KindToken, usdt.Details.Kind)
	assert.Equal(t, "120000000000.5", usdt.Details.CirculatingSupply.String())
	assert.Equal(t, []source.Contract{{Network: "ethereum", Address: "0xdAC17F958D2ee523a2206206994597C13D831ec7"}}, usdt.Contracts)
	assert.Equal(t, marketdata.KindCoin, assets[0].Details.Kind)
}

func TestRecords_ReadInOneRequest(t *testing.T) {
	records, err := newSource(t).Records(context.Background(), []string{"1", "825"})
	require.NoError(t, err)
	require.Len(t, records, 2)
	usdt := records["825"]
	assert.Equal(t, []source.Contract{
		{Network: "bsc", Address: "0x55d398326f99059ff775485246999027b3197955"},
		{Network: "ethereum", Address: "0xdac17f958d2ee523a2206206994597c13d831ec7"},
		{Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
	}, usdt.Contracts, "the networks get the catalog's names")
	assert.Equal(t, "https://tether.to", usdt.Details.Links["website"])
	assert.Equal(t, "https://twitter.com/tether_to", usdt.Details.Links["twitter"])
	assert.Equal(t, marketdata.KindCoin, records["1"].Details.Kind)

	_, err = newSource(t).Records(context.Background(), []string{"1;drop"})
	assert.ErrorIs(t, err, source.ErrNotFound)
}
