package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

// requestUser is the user or administrator of a signed-in request, for the
// request log (#50).
func requestUser(c *gin.Context) string {
	if claims, ok := auth.GetClaims(c); ok {
		return claims.UUID
	}
	return ""
}

// startMetrics serves the metrics of the instance on
// observability.metrics.port at observability.metrics.path (#50), with the
// gauges of its services, when observability.metrics.enabled is set. It
// returns the worker that stops the server, or nil when the metrics are off.
func startMetrics(services *models.InfraServices, handlers *InfraHandlers) (*worker, error) {
	m := config.Config.Observability.Metrics
	if !m.Enabled {
		return nil, nil
	}
	if err := registerServiceGauges(services, handlers); err != nil {
		return nil, err
	}
	server, err := metrics.Listen(net.JoinHostPort("", strconv.Itoa(m.Port)), m.Path)
	if err != nil {
		return nil, err
	}
	server.Start()
	log.Printf("Serving the metrics on %s%s", server.Addr(), m.Path)
	return &worker{"metrics server", server.Stop}, nil
}

// pingTimeout bounds the ping of a service at a scrape.
const pingTimeout = time.Second

// registerServiceGauges registers the gauges that a scrape reads from the
// services: the pool of PostgreSQL, whether Redis and RabbitMQ answer, and
// the WebSocket connections of this instance. A gauge that is registered
// already, as in a test that wires the server twice, is kept.
func registerServiceGauges(services *models.InfraServices, handlers *InfraHandlers) error {
	var errs []error
	gauge := func(name, help string, read func() float64) {
		if err := metrics.Gauge(name, help, read); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				errs = append(errs, err)
			}
		}
	}
	if services.Postgres != nil && services.Postgres.DB != nil {
		pool := services.Postgres.DB
		gauge("db_connections_open", "Connections of the PostgreSQL pool, in use or idle.",
			func() float64 { return float64(pool.Stats().OpenConnections) })
		gauge("db_connections_in_use", "Connections of the PostgreSQL pool in use.",
			func() float64 { return float64(pool.Stats().InUse) })
		gauge("db_connections_idle", "Idle connections of the PostgreSQL pool.",
			func() float64 { return float64(pool.Stats().Idle) })
		gauge("db_connection_waits", "Times a query waited for a connection of the pool, since the start.",
			func() float64 { return float64(pool.Stats().WaitCount) })
		gauge("db_connection_wait_seconds", "Time that queries waited for a connection of the pool, since the start.",
			func() float64 { return pool.Stats().WaitDuration.Seconds() })
	}
	if services.Redis != nil {
		redis := services.Redis
		gauge("redis_up", "1 when Redis answers a ping within a second, 0 otherwise.", func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			defer cancel()
			return up(redis.Ping(ctx) == nil)
		})
	}
	if services.RabbitMQ != nil {
		broker := services.RabbitMQ
		gauge("rabbitmq_up", "1 when the connection to RabbitMQ and its publishing channel are up, 0 otherwise.",
			func() float64 { return up(broker.IsHealthy()) })
	}
	if handlers != nil && handlers.Socket != nil {
		ws := handlers.Socket
		gauge("websocket_connections", "Open WebSocket connections of this instance.",
			func() float64 { return float64(ws.ConnectionCount()) })
	}
	if len(errs) > 0 {
		return fmt.Errorf("register the gauges: %w", errors.Join(errs...))
	}
	return nil
}

func up(ok bool) float64 {
	if ok {
		return 1
	}
	return 0
}
