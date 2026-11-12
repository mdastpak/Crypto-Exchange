// Package metrics holds the Prometheus metrics of the server (#50) and
// serves them on an address of their own, observability.metrics.port, apart
// from the API, so that they are never reachable from the public network
// through the API's address. The packages that observe something record it
// on the collectors here; the composition root registers the gauges that it
// reads from the services when a scrape asks for them.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every metric of the process. It is the registry of this
// package, not the default one of Prometheus, so that a library cannot add
// metrics to it unseen.
var Registry = prometheus.NewRegistry()

// The collectors that the packages record on.
var (
	// HTTPRequests counts the answered requests by method, route pattern,
	// and status.
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requests answered, by method, route pattern, and status.",
	}, []string{"method", "route", "status"})
	// HTTPDuration is the time to answer a request, by method and route.
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Time to answer a request, by method and route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	// RateLimitRejections counts the requests and messages that a limiter
	// refused, by limiter.
	RateLimitRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rate_limit_rejections_total",
		Help: "Requests and messages refused by a rate limiter, by limiter.",
	}, []string{"limiter"})
	// PublishDuration is the time to publish a message to RabbitMQ and
	// receive its confirm, by exchange and outcome.
	PublishDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rabbitmq_publish_duration_seconds",
		Help:    "Time to publish a message and receive its publisher confirm, by exchange and outcome (ok or error).",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"exchange", "outcome"})
	// SealFindings counts the rows that failed their integrity check when
	// their finding was opened, by table and check (#139): an alert on
	// any increase finds a change made outside the backend.
	SealFindings = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "row_seal_findings_total",
		Help: "Integrity findings opened, by table and failed check (missing, mismatch, unknown_key, row_missing).",
	}, []string{"table", "check"})

	// LedgerMismatches is the number of balances that the last
	// reconciliation of the ledger found differing from the replay of their
	// changes (#60), and LedgerImbalancedCurrencies the number of
	// currencies whose books did not sum to zero, or whose custody held
	// other than it must. Any value above zero is a change made outside
	// the ledger: alert on it.
	LedgerMismatches = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_mismatches",
		Help: "Balances that differ from the replay of their changes, at the last reconciliation.",
	})
	LedgerImbalancedCurrencies = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_imbalanced_currencies",
		Help: "Currencies whose books do not sum to zero, or whose custody holds other than it must, at the last reconciliation.",
	})
	// LedgerOperations counts the operations of the ledger (#60), by kind
	// and outcome: applied, replayed (a retry of an applied key),
	// insufficient (a balance would go below zero), invalid, conflict (a
	// key of another request), unverified (a balance fails its seal), and
	// failed. LedgerOperationDuration times them, from the first statement
	// to the commit.
	LedgerOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ledger_operations_total",
		Help: "Operations of the ledger, by kind and outcome.",
	}, []string{"kind", "outcome"})
	LedgerOperationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ledger_operation_duration_seconds",
		Help:    "Time of an operation of the ledger, by kind.",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})
	// LedgerLastReconciliation is the Unix time of the last reconciliation
	// that completed: alert when it is too old.
	LedgerLastReconciliation = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_last_success_timestamp_seconds",
		Help: "Unix time of the last completed reconciliation of the ledger.",
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, RateLimitRejections, PublishDuration, SealFindings,
		LedgerMismatches, LedgerImbalancedCurrencies, LedgerLastReconciliation, LedgerOperations, LedgerOperationDuration,
	)
}

// Middleware records every request on HTTPRequests and HTTPDuration, by
// its route pattern, such as /api/v1/user/contacts/:id, so that a value of
// the path never becomes a label; a request that matched no route is
// "unmatched".
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		HTTPDuration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
		HTTPRequests.WithLabelValues(c.Request.Method, route, strconv.Itoa(c.Writer.Status())).Inc()
	}
}

// Gauge registers a gauge whose value read returns at each scrape. It
// returns an error for a name that is registered already.
func Gauge(name, help string, read func() float64) error {
	return Registry.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, read))
}

// Server serves the metrics on an address of its own.
type Server struct {
	srv      *http.Server
	listener net.Listener
}

// Listen binds the metrics to addr, at path.
func Listen(addr, path string) (*Server, error) {
	mux := http.NewServeMux()
	mux.Handle(path, promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for the metrics on %s: %w", addr, err)
	}
	return &Server{srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, listener: l}, nil
}

// Addr is the address that the server listens on.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Start serves until Stop.
func (s *Server) Start() {
	go func() {
		if err := s.srv.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Error serving the metrics: %v", err)
		}
	}()
}

// Stop ends the server, waiting for the scrapes in progress until ctx ends.
func (s *Server) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
