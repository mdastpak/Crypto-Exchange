// Package dberr tells a request that PostgreSQL refused it for a moment
// (#230): a lock wait or a statement past its timeout, a deadlock, a
// serialization failure, too many connections, or a node that only reads
// during a failover. Those are not bugs, and the client may retry, so the
// response is 503 database_busy with Retry-After rather than 500.
//
// The handlers do not inspect every error. Track gives a request's context
// a note; the database tracer of internal/db records each busy error of a
// query run with that context; and the response helpers turn a 500 of a
// noted request into 503. A durable Redis write that its replicas did not
// acknowledge is noted the same way, through MarkBusy (#231), and a
// dependency that is away, such as RabbitMQ, through MarkUnavailable, with
// an answer of its own (#232); every command of the Redis clients that
// cannot reach Redis notes StoreUnavailable (#130).
package dberr

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
)

// Code is the error code of a response refused because the database was
// busy.
const Code = "database_busy"

// RetryAfterSeconds is the Retry-After of such a response.
const RetryAfterSeconds = 1

// busyStates are the SQLSTATEs of the refusals that a retry may pass.
var busyStates = map[string]bool{
	"55P03": true, // lock_not_available: lock_timeout
	"57014": true, // query_canceled: statement_timeout
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"53300": true, // too_many_connections
	"25006": true, // read_only_sql_transaction: a demoted primary
	"57P01": true, // admin_shutdown: the node is going down
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now: the node is starting
}

// IsBusy reports whether err is a refusal of PostgreSQL that a retry may
// pass.
func IsBusy(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && busyStates[pgErr.Code]
}

// IsReadOnly reports whether err says that the node no longer writes,
// as a primary that a failover demoted.
func IsReadOnly(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "25006"
}

type noteKey struct{}

// note records whether a request met a busy database, or a dependency that
// was away.
type note struct {
	busy        atomic.Bool
	unavailable atomic.Pointer[Unavailable]
}

// Track returns ctx with a note, for one request.
func Track(ctx context.Context) context.Context {
	return context.WithValue(ctx, noteKey{}, &note{})
}

// Record notes err on the request of ctx, if it is a busy refusal and ctx
// is tracked.
func Record(ctx context.Context, err error) {
	if err == nil || !IsBusy(err) {
		return
	}
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.busy.Store(true)
	}
}

// MarkBusy notes on the request of ctx, if it is tracked, that a data store
// refused it for a moment for a reason that is not a PostgreSQL error: a
// durable Redis write that its replicas did not acknowledge in time (#231).
func MarkBusy(ctx context.Context) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.busy.Store(true)
	}
}

// Busy reports whether a query of the request of ctx was refused by a busy
// database.
func Busy(ctx context.Context) bool {
	n, ok := ctx.Value(noteKey{}).(*note)
	return ok && n.busy.Load()
}

// Unavailable is the answer to a request that failed because a dependency
// it needs was away, such as RabbitMQ for the delivery of a sign-in code
// (#232): 503 with Code, Message, and Retry-After.
type Unavailable struct {
	Code              string
	Message           string
	RetryAfterSeconds int
}

// CodeDeliveryUnavailable answers a request for a code that cannot be
// queued for delivery, because RabbitMQ is away.
const CodeDeliveryUnavailable = "delivery_unavailable"

// DeliveryUnavailable is the answer of CodeDeliveryUnavailable. A broker
// restart or a failover to another node takes seconds.
var DeliveryUnavailable = Unavailable{
	Code:              CodeDeliveryUnavailable,
	Message:           "Codes cannot be sent right now. Please try again in a moment.",
	RetryAfterSeconds: 5,
}

// CodeServiceUnavailable answers a request that failed because Redis could
// not be reached (#130): a token whose revocation cannot be checked, a
// sign-in code that cannot be stored or checked, or a cache that cannot be
// read. The request is still refused, as before, but with 503 and
// Retry-After instead of 500, so that clients and load balancers retry,
// and the server's own error rate stays meaningful.
const CodeServiceUnavailable = "service_unavailable"

// StoreUnavailable is the answer of CodeServiceUnavailable. A Redis
// failover takes seconds; a client that retries every second meets the
// new primary soon after it is promoted.
var StoreUnavailable = Unavailable{
	Code:              CodeServiceUnavailable,
	Message:           "The service is temporarily unavailable. Please try again in a moment.",
	RetryAfterSeconds: 1,
}

// MarkUnavailable notes on the request of ctx, if it is tracked, that a
// dependency it needs was away, and how to answer it. The response helpers
// answer a 500 of such a request with it.
func MarkUnavailable(ctx context.Context, answer Unavailable) {
	if n, ok := ctx.Value(noteKey{}).(*note); ok {
		n.unavailable.Store(&answer)
	}
}

// UnavailableOf returns the answer that MarkUnavailable noted on the
// request of ctx.
func UnavailableOf(ctx context.Context) (Unavailable, bool) {
	n, ok := ctx.Value(noteKey{}).(*note)
	if !ok {
		return Unavailable{}, false
	}
	answer := n.unavailable.Load()
	if answer == nil {
		return Unavailable{}, false
	}
	return *answer, true
}
