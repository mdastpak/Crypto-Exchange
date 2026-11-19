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

	"github.com/mdastpak/crypto-exchange/internal/httpmethod"

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
	// currencies whose books did not sum to zero, whose custody held
	// other than it must, or whose accounts have a problem. Any value above zero is a change made outside
	// the ledger: alert on it.
	LedgerMismatches = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_mismatches",
		Help: "Balances that differ from the replay of their changes, at the last reconciliation.",
	})
	LedgerImbalancedCurrencies = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_imbalanced_currencies",
		Help: "Currencies whose books do not sum to zero, or whose custody holds other than it must, at the last reconciliation.",
	})
	// LedgerAccountProblems is the number of accounts of the exchange that
	// the last reconciliation found in a state that the operations of the
	// ledger cannot leave (#404), such as fees below zero: a balanced entry
	// written outside the ledger. Alert on any value above zero.
	LedgerAccountProblems = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ledger_reconciliation_account_problems",
		Help: "Accounts of the exchange in a state that the ledger's operations cannot leave, at the last reconciliation.",
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
	// SMSSends counts the messages handed to each SMS provider (#371), by
	// result: accepted, provider_error (the provider failed; the next one
	// is tried), and refused (the message itself, such as an invalid
	// number).
	SMSSends = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_sms_sends_total",
		Help: "Messages handed to an SMS provider, by provider and result.",
	}, []string{"provider", "result"})
	// SMSFallbacks counts the messages that went through another provider
	// than the first: reason send when the first failed to take it,
	// delivery when it took it and did not deliver it.
	SMSFallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_sms_fallbacks_total",
		Help: "Messages sent through another SMS provider, by the one that failed, the one that sent, and why.",
	}, []string{"from", "to", "reason"})
	// SMSDeliveryReports counts the delivery reports of the messages sent,
	// by provider and final state: delivered, failed, unknown, or pending
	// after the last check.
	SMSDeliveryReports = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_sms_delivery_reports_total",
		Help: "Delivery reports of SMS, by provider and state.",
	}, []string{"provider", "state"})
	// PriceTicks counts the ticks that the price providers sent and the
	// aggregator kept, by provider and kind (book or trade) (#58).
	PriceTicks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "price_ticks_total",
		Help: "Ticks of the price providers that the aggregator kept, by provider and kind.",
	}, []string{"provider", "kind"})
	// PriceTicksRejected counts the ticks that the aggregator rejected, by
	// provider and reason.
	PriceTicksRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "price_ticks_rejected_total",
		Help: "Ticks of the price providers that the aggregator rejected, by provider and reason.",
	}, []string{"provider", "reason"})
	// PriceProviderLag is the delay of a provider's last tick that carried
	// the provider's time, and PriceProviderAge the time since its last
	// tick of any kind.
	PriceProviderLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_provider_lag_seconds",
		Help: "Delay between a price provider's time of its last tick and its receipt.",
	}, []string{"provider"})
	PriceProviderAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_provider_last_tick_age_seconds",
		Help: "Time since the last tick of a price provider.",
	}, []string{"provider"})
	// PriceProviderConnected is 1 while a provider's stream is open, and
	// PriceProviderFallback 1 while its REST API is read instead.
	PriceProviderConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_provider_connected",
		Help: "1 while the stream of a price provider is open.",
	}, []string{"provider"})
	PriceProviderFallback = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_provider_rest_fallback",
		Help: "1 while a price provider is read through its REST API.",
	}, []string{"provider"})
	// PriceAggregation is the time of one aggregation of every market.
	PriceAggregation = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "price_aggregation_duration_seconds",
		Help:    "Time of one aggregation of the prices of every market.",
		Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1},
	})
	// PriceMarketLive is 1 while a market's price is live, and
	// PriceMarketSources the providers that make it.
	PriceMarketLive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_market_live",
		Help: "1 while the reference price of a market is live, 0 while it is stale.",
	}, []string{"market"})
	PriceMarketSources = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "price_market_sources",
		Help: "Providers that make the reference price of a market.",
	}, []string{"market"})
	// PricePublisherLeader is 1 on the replica that publishes the updates
	// (#59), and PricePublishErrors counts the failed writes, by target
	// (cache, lease, stream, rabbitmq).
	PricePublisherLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "price_publisher_leader",
		Help: "1 on the replica of the price aggregator that publishes the updates.",
	})
	PricePublishErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "price_publish_errors_total",
		Help: "Failed writes of the prices, by target: cache, lease, stream, rabbitmq.",
	}, []string{"target"})
	// PriceSignatureFailures counts the prices read from Redis that were
	// refused for their signature (#450), by reason: unsigned,
	// unknown_key, malformed, or bad_signature.
	PriceSignatureFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "price_signature_failures_total",
		Help: "Prices read from Redis and refused for their signature, by reason: unsigned, unknown_key, malformed, bad_signature.",
	}, []string{"reason"})
	// ConversionFailures counts the forced conversions of balances that
	// failed and were held for a retry (#452), by kind (currency_migration
	// or currency_delisting) and code.
	ConversionFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "currency_conversion_failures_total",
		Help: "Forced conversions of balances that failed and were held for a retry, by kind and code.",
	}, []string{"kind", "code"})
	// The notices of delistings (#461): those due and not prepared yet,
	// those still broadcasting to the other users, the recipients waiting
	// by holder, with the age of the oldest due one from its notice, and
	// the failures, by stage (prepare or send). Each instance reads the
	// same tables: alert on max().
	DelistingNoticesUnprepared = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "delisting_notices_unprepared",
		Help: "Notices of delistings that are due and whose recipients are not written yet.",
	})
	DelistingNoticeBroadcasts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "delisting_notice_broadcasts_pending",
		Help: "Notices of delistings to every user whose other users are still being sent.",
	})
	DelistingNoticeRecipientsPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "delisting_notice_recipients_pending",
		Help: "Recipients of notices of delistings not sent yet, by holder.",
	}, []string{"holder"})
	DelistingNoticeOldestPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "delisting_notice_oldest_pending_seconds",
		Help: "How long ago the notice of the oldest due recipient not sent yet was due, by holder.",
	}, []string{"holder"})
	DelistingNoticeFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "delisting_notice_failures_total",
		Help: "Notices of delistings that failed to prepare, and sends to a recipient that failed, by stage.",
	}, []string{"stage"})
	// PriceDelivery is the time from a price's newest tick to its send to
	// the WebSocket clients of an instance (#59).
	PriceDelivery = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "price_delivery_latency_seconds",
		Help:    "Time from the newest provider tick of a price to its send to the WebSocket clients.",
		Buckets: []float64{.005, .01, .025, .05, .1, .15, .2, .25, .5, 1, 2.5, 5},
	})
	// OTCQuotes counts the quote requests (#61), by market and outcome:
	// quoted, or the code of the refusal. A market that is not quoted is
	// "other", so that a client cannot add labels.
	OTCQuotes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "otc_quotes_total",
		Help: "OTC quote requests, by market and outcome: quoted, or the code of the refusal.",
	}, []string{"market", "outcome"})
	// OTCQuoteDuration is the time to price and store a quote.
	OTCQuoteDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "otc_quote_duration_seconds",
		Help:    "Time to answer an OTC quote request.",
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
	})
	// OTCQuotedMargin is the margin of the quotes given, in basis points,
	// by market and side: with the realized margin of the execution (#62),
	// it shows whether the quotes earn what they should.
	OTCQuotedMargin = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otc_quoted_margin_bps",
		Help:    "Margin of the OTC quotes given, in basis points of the reference, by market and side.",
		Buckets: []float64{0, 5, 10, 20, 30, 50, 75, 100, 150, 200, 500},
	}, []string{"market", "side"})
	// OTCRoutes counts the routings of the OTC trades (#509), by stage
	// (quote or execution), rule, and outcome: routed, or the reason that
	// there is no route. A trade with no route has the rule "none". The
	// rules and the reasons are few: an operator writes the rules.
	OTCRoutes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "otc_route_total",
		Help: "Routings of OTC trades, by stage (quote, execution), rule, and outcome: routed, or the reason of no route.",
	}, []string{"stage", "rule", "outcome"})
	// OTCRouteLegs counts the legs of the routes found (#509), by stage,
	// source (inventory or venue), and venue: where the liquidity comes
	// from.
	OTCRouteLegs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "otc_route_legs_total",
		Help: "Legs of the OTC routes found, by stage (quote, execution), source (inventory, venue), and venue.",
	}, []string{"stage", "source", "venue"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, RateLimitRejections, PublishDuration, SealFindings,
		LedgerMismatches, LedgerImbalancedCurrencies, LedgerAccountProblems, LedgerLastReconciliation, LedgerOperations, LedgerOperationDuration,
		SMSSends, SMSFallbacks, SMSDeliveryReports,
		PriceTicks, PriceTicksRejected, PriceProviderLag, PriceProviderAge, PriceProviderConnected, PriceProviderFallback,
		PriceAggregation, PriceMarketLive, PriceMarketSources, PricePublisherLeader, PricePublishErrors, PriceSignatureFailures, PriceDelivery, ConversionFailures,
		DelistingNoticesUnprepared, DelistingNoticeBroadcasts, DelistingNoticeRecipientsPending, DelistingNoticeOldestPending, DelistingNoticeFailures,
		OTCQuotes, OTCQuoteDuration, OTCQuotedMargin, OTCRoutes, OTCRouteLegs,
	)
}

// Middleware records every request on HTTPRequests and HTTPDuration, by
// its route pattern, such as /api/v1/user/contacts/:id, so that a value of
// the path never becomes a label; a request that matched no route is
// "unmatched". The method is one of the standard ones, or "other" (#403),
// since a client chooses it.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := httpmethod.Normalize(c.Request.Method)
		HTTPDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
		HTTPRequests.WithLabelValues(method, route, strconv.Itoa(c.Writer.Status())).Inc()
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
