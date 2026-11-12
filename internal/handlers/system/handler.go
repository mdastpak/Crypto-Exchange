package system

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/models"
)

// Pinger is a dependency whose health a ping checks: *db.PostgresService
// and *db.RedisService.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Broker reports the health of the message broker: *rabbitmq.RabbitMQService.
type Broker interface {
	IsHealthy() bool
}

// GeoIPCheck reports the health of the country database: *geoip.Resolver.
type GeoIPCheck interface {
	Check(now time.Time) error
}

// EvictionCheck reports keys that Redis evicted: *redisutil.EvictionWatch.
type EvictionCheck interface {
	Check(ctx context.Context) error
}

// SystemHandler answers the health probes. It depends on interfaces only
// (#48); a nil dependency is not checked, or is reported as down.
type SystemHandler struct {
	Postgres        Pinger
	Redis           Pinger
	RabbitMQService Broker
	// GeoIP is checked when it is set (#284): a database that is missing,
	// failed to reload, or is stale makes the instance degraded, never
	// unready, since it only makes the suggested site worse.
	GeoIP GeoIPCheck
	// Eviction is checked when it is set (#305): keys that Redis evicted
	// since the instance started make it degraded. Evicted keys may have
	// been logged-out tokens or attempt counters, which an operator must
	// look into, but the instance can still serve.
	Eviction EvictionCheck

	// checks replaces the dependency checks in tests.
	checks []dependency
	// checkTimeout bounds each check; zero means defaultCheckTimeout.
	checkTimeout time.Duration
	// draining is set when the instance starts to shut down.
	draining atomic.Bool
}

// StartDraining makes readiness answer 503 from now on, without checking
// the dependencies, so that load balancers stop sending requests to an
// instance that is shutting down. Liveness keeps answering 200.
func (h *SystemHandler) StartDraining() {
	h.draining.Store(true)
}

// NewHandler creates a new system handler with injected dependencies

// NewHandler returns the handler of the services in is. A service that is
// not there stays nil, never a typed nil, so that the checks see it.
func NewHandler(is *models.InfraServices) *SystemHandler {
	h := &SystemHandler{}
	if is.Postgres != nil {
		h.Postgres = is.Postgres
	}
	if is.Redis != nil {
		h.Redis = is.Redis
	}
	if is.RabbitMQ != nil {
		h.RabbitMQService = is.RabbitMQ
	}
	return h
}
