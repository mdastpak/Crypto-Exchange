package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientFromConfig_TakesTheRouteKeyAndAddressOfTheNode(t *testing.T) {
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })

	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Cg-Demo-Api-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	config.Config.MarketData = config.MarketDataConfig{
		Egress: config.MarketDataEgress{Default: config.EgressNone},
		Sources: map[string]config.MarketDataSource{
			"coingecko": {Enabled: true, Egress: config.EgressDirect, APIKey: "demo-key", BaseURL: srv.URL, TimeoutSeconds: 5},
			"nobitex":   {Enabled: true, TimeoutSeconds: 5},
			"binance":   {Enabled: true, Egress: config.EgressProxy, TimeoutSeconds: 5},
		},
	}
	config.Config.MarketData.Egress.ProxyURL = "socks5://gw.example:1080"

	c, err := ClientFromConfig(&config.Config, nil, "coingecko", Settings{DefaultBaseURL: "https://api.coingecko.com/api/v3", KeyHeader: "x-cg-demo-api-key"})
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, c.http.Timeout)
	_, err = c.Get(context.Background(), "ping", nil)
	require.NoError(t, err)
	assert.Equal(t, "demo-key", gotKey, "the key travels in its header")

	_, err = ClientFromConfig(&config.Config, nil, "nobitex", Settings{DefaultBaseURL: "https://api.nobitex.ir"})
	assert.ErrorContains(t, err, "does not call it", "the node's default route is none")

	proxied, err := ClientFromConfig(&config.Config, nil, "binance", Settings{DefaultBaseURL: "https://api.binance.com"})
	require.NoError(t, err)
	proxyURL, err := proxied.http.Transport.(*http.Transport).Proxy(httptest.NewRequest(http.MethodGet, "https://api.binance.com", nil))
	require.NoError(t, err)
	assert.Equal(t, "socks5://gw.example:1080", proxyURL.String())
	assert.Nil(t, proxied.opts.Limiter, "a plan without limits makes no count")
}
