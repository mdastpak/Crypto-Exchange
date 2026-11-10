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

// dependency is a service that the instance uses to serve traffic.
type dependency struct {
	name  string
	check func(ctx context.Context) error
	// optional marks a service that only some routes need, so that the
	// instance stays ready without it: RabbitMQ, which only the delivery of
	// sign-in codes needs at once (#232). The currency events wait in the
	// outbox, and a sign-in code request answers 503 while it is away.
	optional bool
}

var errUnavailable = errors.New("unavailable")

// dependencies returns the checks of PostgreSQL, Redis, and RabbitMQ. A
// service that is not configured counts as unavailable.
func (h *SystemHandler) dependencies() []dependency {
	if h.checks != nil {
		return h.checks
	}
	deps := []dependency{
		{"postgres", func(ctx context.Context) error {
			if h.Postgres == nil {
				return errUnavailable
			}
			return h.Postgres.Ping(ctx)
		}, false},
		{"redis", func(ctx context.Context) error {
			if h.Redis == nil {
				return errUnavailable
			}
			return h.Redis.Ping(ctx)
		}, false},
		{"rabbitmq", func(ctx context.Context) error {
			if h.RabbitMQService == nil || !h.RabbitMQService.IsHealthy() {
				return errUnavailable
			}
			return nil
		}, true},
	}
	if h.GeoIP != nil {
		deps = append(deps, dependency{"geoip", func(context.Context) error {
			return h.GeoIP.Check(time.Now())
		}, true})
	}
	if h.Eviction != nil {
		deps = append(deps, dependency{"redis_eviction", h.Eviction.Check, true})
	}
	return deps
}

// LivenessHandler reports that the process serves requests. It checks no
// dependency, so that an orchestrator does not restart an instance for an
// outage elsewhere.
//
// @Summary Liveness
// @Description Whether the process serves requests. Checks no dependency.
// @Tags system
// @Produce json
// @Success 200 {object} HealthStatus
// @Router /system/live [get]
func (h *SystemHandler) LivenessHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ReadinessHandler checks every dependency at once and answers 200 when
// PostgreSQL and Redis are available, and 503 otherwise, so that load
// balancers and orchestrators stop sending traffic to an instance that
// cannot serve it. Without RabbitMQ the instance stays ready, with the
// status degraded (#232): every instance shares the broker, so taking them
// all out of service would turn a broker outage into an outage of the API.
// The details per dependency are left out in release mode, where they would
// only tell outsiders about the internals.
//
// @Summary Readiness
// @Description 503 while PostgreSQL or Redis is unavailable, or while the instance drains at shutdown. Without RabbitMQ it answers 200 with the status degraded: only the delivery of sign-in codes needs the broker.
// @Tags system
// @Produce json
// @Success 200 {object} HealthStatus
// @Failure 503 {object} HealthStatus
// @Router /system/ready [get]
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
			if !dep.optional {
				code = http.StatusServiceUnavailable
			}
		}
	}
	if gin.Mode() == gin.ReleaseMode {
		health.Details = nil
	}
	c.JSON(code, health)
}
