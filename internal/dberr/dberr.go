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
// acknowledge is noted the same way, through MarkBusy (#231).
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

// note records whether a request met a busy database.
type note struct{ busy atomic.Bool }

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
