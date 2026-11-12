package tracing

import (
	"context"
	"net"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Middleware starts the span of every request, a child of the traceparent
// of the client when it sends one, named by the method and the pattern of
// its route, such as "POST /api/v1/admin/currencies", so that a value of
// the path never names a span. The handlers run under it: the queries and
// the commands of a request are its children. A server error marks the
// span as failed.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		ctx, span := Tracer().Start(ctx, c.Request.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", route),
			))
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, strconv.Itoa(status))
		}
	}
}

// maxStatement bounds the text of a query on its span. The queries hold no
// values: they come as parameters.
const maxStatement = 1000

// QueryStart starts the span of a PostgreSQL query, for a pgx tracer.
func QueryStart(ctx context.Context, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	if len(sql) > maxStatement {
		sql = sql[:maxStatement]
	}
	ctx, _ = Tracer().Start(ctx, "postgresql", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "postgresql"), attribute.String("db.query.text", sql)))
	return ctx
}

// QueryEnd ends the span that QueryStart started.
func QueryEnd(ctx context.Context, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, "query failed")
	}
	span.End()
}

// RedisHook is the go-redis hook that records a span of each command and
// pipeline, named by the command, without its arguments, which may hold
// values such as codes.
type RedisHook struct{}

var _ redis.Hook = RedisHook{}

// DialHook leaves dials untraced.
func (RedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

// ProcessHook records a command.
func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		ctx, span := Tracer().Start(ctx, "redis "+cmd.Name(), trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system.name", "redis"), attribute.String("db.operation.name", cmd.Name())))
		defer span.End()
		err := next(ctx, cmd)
		if err != nil && err != redis.Nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "command failed")
		}
		return err
	}
}

// ProcessPipelineHook records a pipeline, or a transaction, as one span.
func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		ctx, span := Tracer().Start(ctx, "redis pipeline", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system.name", "redis"), attribute.Int("db.operation.batch.size", len(cmds))))
		defer span.End()
		err := next(ctx, cmds)
		if err != nil && err != redis.Nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "pipeline failed")
		}
		return err
	}
}

// StartPublish starts the span of a message published to exchange.
func StartPublish(ctx context.Context, exchange string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "publish "+exchange, trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("messaging.system", "rabbitmq"), attribute.String("messaging.destination.name", exchange)))
}

// End ends span, marked as failed with err when err is set.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed")
	}
	span.End()
}
