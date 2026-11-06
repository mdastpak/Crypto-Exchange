package system

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type HealthStatus struct {
	Status  string                   `json:"status"`
	Details map[string]ServiceStatus `json:"details,omitempty"`
}

type ServiceStatus struct {
	Status    string    `json:"status"`
	LastCheck time.Time `json:"last_check"`
	LatencyMs int64     `json:"latency_ms,omitempty"`
}

// defaultCheckTimeout bounds each dependency check, so that an unreachable
// dependency is reported quickly instead of holding the probe.
const defaultCheckTimeout = 2 * time.Second

// dependency is a service that the instance needs to serve traffic.
type dependency struct {
	name  string
	check func(ctx context.Context) error
}

var errUnavailable = errors.New("unavailable")

// dependencies returns the checks of PostgreSQL, Redis, and RabbitMQ. A
// service that is not configured counts as unavailable.
func (h *SystemHandler) dependencies() []dependency {
	if h.checks != nil {
		return h.checks
	}
	return []dependency{
		{"postgres", func(ctx context.Context) error {
			if h.Postgres == nil {
				return errUnavailable
			}
			return h.Postgres.Ping(ctx)
		}},
		{"redis", func(ctx context.Context) error {
			if h.Redis == nil {
				return errUnavailable
			}
			return h.Redis.Ping(ctx)
		}},
		{"rabbitmq", func(ctx context.Context) error {
			if h.RabbitMQService == nil || !h.RabbitMQService.IsHealthy() {
				return errUnavailable
			}
			return nil
		}},
	}
}

// LivenessHandler reports that the process serves requests. It checks no
// dependency, so that an orchestrator does not restart an instance for an
// outage elsewhere.
func (h *SystemHandler) LivenessHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ReadinessHandler checks every dependency at once and answers 200 when all
// of them are available, and 503 otherwise, so that load balancers and
// orchestrators stop sending traffic to an instance that cannot serve it.
// The details per dependency are left out in release mode, where they would
// only tell outsiders about the internals.
func (h *SystemHandler) ReadinessHandler(c *gin.Context) {
	if h.draining.Load() {
		c.JSON(http.StatusServiceUnavailable, HealthStatus{Status: "draining"})
		return
	}
	timeout := h.checkTimeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	deps := h.dependencies()
	results := make([]ServiceStatus, len(deps))
	var wg sync.WaitGroup
	for i, dep := range deps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
			defer cancel()
			start := time.Now()
			status := "ok"
			if err := dep.check(ctx); err != nil {
				status = "error"
			}
			results[i] = ServiceStatus{Status: status, LastCheck: time.Now().UTC(), LatencyMs: time.Since(start).Milliseconds()}
		}()
	}
	wg.Wait()

	health := HealthStatus{Status: "ok", Details: make(map[string]ServiceStatus, len(deps))}
	code := http.StatusOK
	for i, dep := range deps {
		health.Details[dep.name] = results[i]
		if results[i].Status != "ok" {
			health.Status = "degraded"
			code = http.StatusServiceUnavailable
		}
	}
	if gin.Mode() == gin.ReleaseMode {
		health.Details = nil
	}
	c.JSON(code, health)
}

// HealthCheckHandler serves /system/healthcheck, the earlier name of
// /system/ready.
func (h *SystemHandler) HealthCheckHandler(c *gin.Context) {
	h.ReadinessHandler(c)
}

func (h *SystemHandler) PingPongHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "pong", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}
