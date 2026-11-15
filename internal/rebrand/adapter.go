package rebrand

import (
	"context"
	"database/sql"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
)

// Store is the rebrands of a database, as an object that the handlers hold
// through an interface (#48). Each method is the function of the same name.
type Store struct {
	DB *sql.DB
}

// Schedule is Schedule on s.DB.
func (s Store) Schedule(ctx context.Context, targetHK string, p Plan, origin audit.Event) (Migration, error) {
	return Schedule(ctx, s.DB, targetHK, p, origin)
}

// List is List on s.DB.
func (s Store) List(ctx context.Context, currencyHK string) ([]Migration, error) {
	return List(ctx, s.DB, currencyHK)
}

// Cancel is Cancel on s.DB.
func (s Store) Cancel(ctx context.Context, targetHK, hk string, version *int64, origin audit.Event) (Migration, []currencies.Change, error) {
	return Cancel(ctx, s.DB, targetHK, hk, version, origin)
}

// ConvertOwn is ConvertOwn on s.DB.
func (s Store) ConvertOwn(ctx context.Context, migrationHK, sourceHK string, a ledger.Account) (Converted, error) {
	return ConvertOwn(ctx, s.DB, migrationHK, sourceHK, a)
}

// Progress is Progress on s.DB.
func (s Store) Progress(ctx context.Context, targetHK, migrationHK string) ([]SourceProgress, error) {
	return Progress(ctx, s.DB, targetHK, migrationHK)
}
