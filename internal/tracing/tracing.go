// Package tracing records OpenTelemetry traces of the server (#50): a span
// for each request, each PostgreSQL query, each Redis command, and each
// message published to RabbitMQ, sent with OTLP over HTTP to
// observability.tracing.endpoint, of a share of the traces,
// observability.tracing.sample_rate. The trace of a change goes on through
// the outbox: the relay that publishes its event, maybe on another
// instance, continues the trace of the request that wrote it.
//
// Without Setup, the spans go nowhere and cost almost nothing.
package tracing

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// instrumentation names the spans of this code base.
const instrumentation = "github.com/mdastpak/crypto-exchange"

// propagator carries the context of a trace in W3C traceparent headers.
var propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

// Tracer returns the tracer of the code base, of the provider in use.
func Tracer() trace.Tracer { return otel.Tracer(instrumentation) }

// Setup sends the traces of the process to observability.tracing.endpoint
// in cfg, when observability.tracing.enabled is set. A trace that comes with
// a sampled parent, such as the traceparent header of a client, follows its
// parent; the others are sampled at sample_rate. The returned function
// flushes the spans and stops the exporter.
func Setup(ctx context.Context, cfg *config.Configuration) (shutdown func(context.Context) error, err error) {
	t := cfg.Observability.Tracing
	otel.SetTextMapPropagator(propagator)
	if !t.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx, endpointOptions(t.Endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("observability.tracing.endpoint: %w", err)
	}
	return Install(exporter, t.ServiceName, t.SampleRate), nil
}

// endpointOptions are the options of an OTLP HTTP exporter to endpoint: a
// URL such as https://collector:4318, or a host and port, with TLS unless
// the URL says http.
func endpointOptions(endpoint string) []otlptracehttp.Option {
	switch {
	case strings.HasPrefix(endpoint, "http://"):
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint + "/v1/traces"), otlptracehttp.WithInsecure()}
	case strings.HasPrefix(endpoint, "https://"):
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint + "/v1/traces")}
	}
	return []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
}

// Install makes the spans of the process go to exporter, in batches, under
// the service name, sampled at rate (ParentBased). It returns the function
// that flushes and stops it. Tests install an in-memory exporter with it.
func Install(exporter sdktrace.SpanExporter, service string, rate float64) func(context.Context) error {
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagator)
	return provider.Shutdown
}

// Inject returns the traceparent of the span of ctx, to carry the trace
// through a store, such as the outbox, or "" without a sampled span.
func Inject(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier["traceparent"]
}

// Extract returns ctx with the remote span of traceparent as its parent, so
// that the spans started from it continue that trace. An empty or invalid
// traceparent returns ctx as it is.
func Extract(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// InjectHeaders writes the context of the span of ctx into headers, the
// headers of an AMQP message, so that a consumer can continue the trace.
func InjectHeaders(ctx context.Context, headers map[string]interface{}) {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	for k, v := range carrier {
		headers[k] = v
	}
}
