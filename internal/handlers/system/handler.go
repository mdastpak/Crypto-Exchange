package system

import (
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
)

// SystemHandler struct for system-related endpoints
type SystemHandler struct {
	Postgres        *db.PostgresService
	Redis           *db.RedisService
	RabbitMQService *rabbitmq.RabbitMQService

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
func NewHandler(is *models.InfraServices) *SystemHandler {
	return &SystemHandler{
		Postgres:        is.Postgres,
		Redis:           is.Redis,
		RabbitMQService: is.RabbitMQ,
	}
}
