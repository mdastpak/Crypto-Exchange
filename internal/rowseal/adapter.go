package rowseal

import (
	"context"
	"database/sql"
)

// Findings is the integrity findings of a database, as an object that the
// handlers hold through an interface (#48).
type Findings struct {
	DB *sql.DB
}

// List is ListFindings on f.DB.
func (f Findings) List(ctx context.Context, o ListOptions) ([]Finding, error) { return ListFindings(ctx, f.DB, o) }
