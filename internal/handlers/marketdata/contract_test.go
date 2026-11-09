package marketdata

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/openapitest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoutes_MatchTheOpenAPIDocument checks the answers of every route,
// including the errors, against the generated document, without the other
// services that the end-to-end tests of the server need.
func TestRoutes_MatchTheOpenAPIDocument(t *testing.T) {
	document, err := openapitest.Load("../../../docs/swagger.json")
	require.NoError(t, err)
	f := newFixture(t)
	r := newRouter(newHandler(f, nil))
	for _, path := range []string{
		"/api/v1/market-data/assets?limit=1",
		"/api/v1/market-data/assets?limit=0",
		"/api/v1/market-data/assets/tether",
		"/api/v1/market-data/assets/nothing",
		"/api/v1/market-data/assets/bitcoin/venues",
		"/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=1h",
		"/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=USDT&interval=2h",
		"/api/v1/market-data/assets/bitcoin/candles?venue=binance&quote=IRT&interval=1h",
		"/api/v1/market-data/feed/assets?updated_since=2026-01-01T00:00:00Z",
		"/api/v1/market-data/feed/assets?cursor=nothing",
	} {
		res := get(t, r, path)
		route, _, _ := strings.Cut(path, "?")
		documented, problems := document.Check(http.MethodGet, route, res.Code, res.Hdr.Get("Content-Type"), []byte(res.Raw))
		assert.True(t, documented, path)
		assert.Empty(t, problems, path)
	}
}
