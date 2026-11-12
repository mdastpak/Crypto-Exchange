package audit

import (
	"context"
	"database/sql"

	"github.com/mdastpak/crypto-exchange/internal/pagination"
)

// Log is the audit log of a database, as an object that the handlers hold
// through an interface (#48). Each method is the function of the same name.
type Log struct {
	DB *sql.DB
}

// Record is Record on l.DB, outside any transaction of a change.
func (l Log) Record(ctx context.Context, e Event) error { return Record(ctx, l.DB, e) }

// List is List on l.DB.
func (l Log) List(ctx context.Context, f Filter, page pagination.Request) (pagination.Page[Entry], error) {
	return List(ctx, l.DB, f, page)
}
