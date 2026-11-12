package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"

	"github.com/mdastpak/crypto-exchange/internal/dberr"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// ApplicationName is the application_name of the connections, which
// pg_stat_activity shows, so that the sessions of the server, of cmd/admin,
// and of the migrations can be told apart while a lock is analyzed (#230).
// The commands set it before they connect; the server names its host.
var ApplicationName = "crypto-exchange"

// ServerApplicationName is the application_name of a server instance:
// crypto-exchange-server/<host>.
func ServerApplicationName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return "crypto-exchange-server/" + host
}

// migrationApplicationName is the application_name of the migrations.
func migrationApplicationName() string { return "crypto-exchange-migrate" }

// readOnlyMark is the key of the custom data of a connection that a
// read-only error marked.
const readOnlyMark = "crypto-exchange.read-only"

// openPool opens a pool of cfg. A connection that turns out to be on a node
// that no longer writes is discarded before its next use, so that the pool
// reconnects, through target_session_attrs, to the node that writes now
// (#230): after a failover with a floating address, the pool would
// otherwise keep connections to the demoted primary until their lifetime
// ends.
func openPool(cfg *pgx.ConnConfig, targetSessionAttrs string) *sql.DB {
	wantsWrites := map[string]bool{"read-write": true, "primary": true}[targetSessionAttrs]
	return stdlib.OpenDB(*cfg, stdlib.OptionResetSession(func(_ context.Context, conn *pgx.Conn) error {
		if !wantsWrites {
			return nil
		}
		return checkWritable(conn)
	}))
}

// checkWritable returns driver.ErrBadConn for a connection whose node only
// reads: one that a query found read-only, or whose server reported that
// it is a standby or that transactions are read-only. PostgreSQL 14 and
// later report both settings on every change, so the check sends no query.
func checkWritable(conn *pgx.Conn) error {
	pgConn := conn.PgConn()
	if marked, _ := pgConn.CustomData()[readOnlyMark].(bool); marked {
		return driver.ErrBadConn
	}
	if pgConn.ParameterStatus("in_hot_standby") == "on" || pgConn.ParameterStatus("default_transaction_read_only") == "on" {
		return driver.ErrBadConn
	}
	return nil
}

// busyTracer notes the busy refusals of PostgreSQL on the request of the
// query's context (internal/dberr), and marks a connection whose query met
// a node that only reads, for openPool to discard.
type busyTracer struct{}

func (busyTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (busyTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil {
		return
	}
	dberr.Record(ctx, data.Err)
	if dberr.IsReadOnly(data.Err) && conn != nil {
		conn.PgConn().CustomData()[readOnlyMark] = true
	}
}
