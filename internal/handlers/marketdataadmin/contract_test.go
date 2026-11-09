package marketdataadmin

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/openapitest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoutes_MatchTheOpenAPIDocument checks the answers of the routes,
// including the errors, against the generated document.
func TestRoutes_MatchTheOpenAPIDocument(t *testing.T) {
	document, err := openapitest.Load("../../../docs/swagger.json")
	require.NoError(t, err)
	f := newFixture(t)
	asset, pair := fmt.Sprint(f.btc), fmt.Sprint(f.pair)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/admin/market-data/sources", nil},
		{http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/pause", NoteRequest{Reason: "x"}},
		{http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/pause", nil},
		{http.MethodPost, "/api/v1/admin/market-data/sources/coingecko/resume", nil},
		{http.MethodGet, "/api/v1/admin/market-data/jobs", nil},
		{http.MethodPost, "/api/v1/admin/market-data/jobs/tickers:binance/run", nil},
		{http.MethodPost, "/api/v1/admin/market-data/jobs/nothing/run", nil},
		{http.MethodGet, "/api/v1/admin/market-data/runs?limit=1", nil},
		{http.MethodGet, "/api/v1/admin/market-data/runs?status=lost", nil},
		{http.MethodGet, "/api/v1/admin/market-data/venues", nil},
		{http.MethodPost, "/api/v1/admin/market-data/venues/binance/enable", nil},
		{http.MethodPost, "/api/v1/admin/market-data/venues/binance/disable", nil},
		{http.MethodPost, "/api/v1/admin/market-data/venues/nowhere/enable", nil},
		{http.MethodGet, "/api/v1/admin/market-data/assets?limit=1", nil},
		{http.MethodGet, "/api/v1/admin/market-data/assets/" + asset, nil},
		{http.MethodGet, "/api/v1/admin/market-data/assets/999999", nil},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/hide", NoteRequest{Reason: "x"}},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/unhide", nil},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/link", LinkRequest{CurrencyHK: "0192f3a4-0000-7000-8000-00000000abcd"}},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/link", LinkRequest{CurrencyHK: f.currency}},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/hide", NoteRequest{Reason: "x"}},
		{http.MethodPost, "/api/v1/admin/market-data/assets/" + asset + "/unlink", nil},
		{http.MethodPut, "/api/v1/admin/market-data/assets/" + asset + "/tier", map[string]any{"tier": "hot"}},
		{http.MethodGet, "/api/v1/admin/market-data/pairs?unmapped=true", nil},
		{http.MethodPut, "/api/v1/admin/market-data/pairs/" + pair + "/tier", map[string]any{"tier": nil}},
		{http.MethodPut, "/api/v1/admin/market-data/pairs/" + pair + "/mapping", MappingRequest{BaseAssetID: f.btc, QuoteAssetID: f.usdt}},
		{http.MethodPut, "/api/v1/admin/market-data/pairs/999999/mapping", MappingRequest{BaseAssetID: f.btc, QuoteAssetID: f.usdt}},
		{http.MethodGet, "/api/v1/admin/market-data/conflicts", nil},
		{http.MethodPost, "/api/v1/admin/market-data/conflicts/999999/resolve", ResolveRequest{Note: "x"}},
	} {
		res := f.call(t, c.method, c.path, c.body)
		route, _, _ := strings.Cut(c.path, "?")
		documented, problems := document.Check(c.method, route, res.Code, res.Type, []byte(res.Raw))
		assert.True(t, documented, "%s %s: %d", c.method, c.path, res.Code)
		assert.Empty(t, problems, "%s %s", c.method, c.path)
	}
}
