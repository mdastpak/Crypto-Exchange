package app

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetrics_ExposeTheServicesOfTheInstance scrapes the metrics of an
// instance (#50): the gauges of its PostgreSQL pool, Redis, RabbitMQ, and
// WebSocket connections, a request by its route, and a publish with its
// confirm, on the address of the metrics, apart from the API.
func TestMetrics_ExposeTheServicesOfTheInstance(t *testing.T) {
	env := newE2EEnv(t)
	broker := env.handlers.Socket.RabbitMQService.(*rabbitmq.RabbitMQService)
	services := &models.InfraServices{Postgres: &db.PostgresService{DB: env.db}, Redis: env.handlers.Redis, RabbitMQ: broker}
	require.NoError(t, registerServiceGauges(services, env.handlers))
	require.NoError(t, registerServiceGauges(services, env.handlers), "a second wiring keeps the gauges")
	server, err := metrics.Listen("127.0.0.1:0", "/metrics")
	require.NoError(t, err)
	server.Start()
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	require.Equal(t, http.StatusOK, env.send(t, http.MethodGet, "/api/v1/public/currencies", nil, "", "").Code)
	exchange := config.Config.RabbitMQ.Exchanges.PriceUpdates
	require.NoError(t, broker.Publish(context.Background(), exchange, `{"test": true}`))

	resp, err := http.Get("http://" + server.Addr() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	for _, line := range []string{
		"db_connections_open ",
		"db_connections_in_use ",
		"db_connection_waits ",
		"redis_up 1",
		"rabbitmq_up 1",
		"websocket_connections 0",
		`http_requests_total{method="GET",route="/api/v1/public/currencies",status="200"}`,
		`http_request_duration_seconds_count{method="GET",route="/api/v1/public/currencies"}`,
		`rabbitmq_publish_duration_seconds_count{exchange="` + exchange + `",outcome="ok"}`,
	} {
		assert.Contains(t, text, line)
	}
}
