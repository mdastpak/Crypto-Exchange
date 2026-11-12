package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordSpans records every span of the process in memory until the test
// ends.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exporter
}

// TestTracing_TheCurrencyCreateFlowIsOneTrace creates a currency through the
// route (#50). The span of the request, the PostgreSQL queries of the
// change, and the publish of its event, which the outbox relay sends after
// the commit, are one trace: the outbox carries the trace context.
func TestTracing_TheCurrencyCreateFlowIsOneTrace(t *testing.T) {
	env := newE2EEnv(t)
	admin, err := env.accounts.Create(context.Background(), "tracer@example.com")
	require.NoError(t, err)
	adminToken := env.signInAdmin(t, admin)
	spans := recordSpans(t)

	status, resp := env.post(t, "/api/v1/admin/currencies", testCurrency("TRC"), adminToken)
	require.Equal(t, http.StatusCreated, status, resp.Message)

	var request trace.TraceID
	for _, s := range spans.GetSpans() {
		if s.Name == "POST /api/v1/admin/currencies" {
			request = s.SpanContext.TraceID()
		}
	}
	require.True(t, request.IsValid(), "the request has a span")

	exchange := config.Config.RabbitMQ.Exchanges.PriceUpdates
	require.Eventually(t, func() bool {
		for _, s := range spans.GetSpans() {
			if s.Name == "publish "+exchange && s.SpanContext.TraceID() == request {
				return true
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond, "the relay publishes the event in the trace of the request")

	var queries, insert int
	for _, s := range spans.GetSpans() {
		if s.SpanContext.TraceID() != request || s.Name != "postgresql" {
			continue
		}
		queries++
		for _, a := range s.Attributes {
			if a.Key == "db.query.text" && strings.Contains(a.Value.AsString(), "INSERT INTO currencies") {
				insert++
			}
		}
	}
	assert.Greater(t, queries, 1, "the queries of the change are spans of the trace")
	assert.Equal(t, 1, insert, "the insert of the currency is one of them")
}
