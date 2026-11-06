package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listedSymbols returns the symbols that GET /public/currencies lists.
func listedSymbols(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Info []models.Currency `json:"info"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	symbols := make([]string, 0, len(resp.Info))
	for _, c := range resp.Info {
		symbols = append(symbols, c.Info.Symbol)
	}
	return symbols
}

func listCurrencies(t *testing.T, h http.Handler) []string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public/currencies", nil))
	return listedSymbols(t, w)
}

// TestShutdown_KeepsTheSharedCurrencyCache starts a second server instance
// next to the end-to-end environment, against the same PostgreSQL, Redis,
// and RabbitMQ, as during a rolling deploy, and stops it with the shutdown
// sequence of main. A change through one instance must show on the other at
// once, and the first instance must keep listing the currencies.
func TestShutdown_KeepsTheSharedCurrencyCache(t *testing.T) {
	env := newE2EEnv(t)
	_, err := env.db.Exec(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb)`,
		`{"en_name": "Bitcoin", "fa_name": "Bitcoin", "symbol": "BTC", "if_fiat": false, "is_token": false, "contract": ""}`)
	require.NoError(t, err)

	// The second instance starts as main starts it.
	services, cleanup, err := initializeServices()
	require.NoError(t, err)
	handlers := initializeHandlers(services)
	srv := &http.Server{}
	stopped := false
	// Stops the second instance when the test fails before it does.
	t.Cleanup(func() {
		if !stopped {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = shutdown(ctx, srv, handlers, nil, 0)
			cleanup()
		}
	})
	require.NoError(t, handlers.Socket.StartConsumer(context.Background()))
	router, err := newEngine()
	require.NoError(t, err)
	require.NoError(t, RouteHandler(router, handlers))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv.Handler = router
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	assert.Equal(t, []string{"BTC"}, listCurrencies(t, router), "the second instance serves the currency")
	assert.Equal(t, []string{"BTC"}, listCurrencies(t, env.router), "the first instance serves the currency")

	// Both instances have the list cached now. A change through the first
	// one drops the shared list, so the second shows it at once.
	admin, err := env.accounts.Create(context.Background(), "cache@example.com")
	require.NoError(t, err)
	status, resp := env.post(t, "/admin/currencies", testCurrency("ETH"), env.signInAdmin(t, admin))
	require.Equal(t, http.StatusCreated, status, resp.Message)
	assert.Equal(t, []string{"BTC", "ETH"}, listCurrencies(t, router), "the second instance shows a change made through the first")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped = true
	require.NoError(t, shutdown(ctx, srv, handlers, nil, 0))
	require.ErrorIs(t, <-served, http.ErrServerClosed)
	cleanup()

	assert.Equal(t, []string{"BTC", "ETH"}, listCurrencies(t, env.router),
		"stopping one instance must not change the responses of the others")
}
