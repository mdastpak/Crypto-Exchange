package delisting

import (
	"context"
	"database/sql"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"
)

// Store is the delistings of a database, as an object that the handlers
// hold through an interface (#48). Each method is the function of the same
// name.
type Store struct {
	DB *sql.DB
}

// Schedule is Schedule on s.DB.
func (s Store) Schedule(ctx context.Context, currencyHK string, p Plan, origin audit.Event) (Delisting, Change, error) {
	return Schedule(ctx, s.DB, currencyHK, p, origin)
}

// List is List on s.DB.
func (s Store) List(ctx context.Context, currencyHK string) ([]Delisting, error) {
	return List(ctx, s.DB, currencyHK)
}

// Cancel is Cancel on s.DB.
func (s Store) Cancel(ctx context.Context, currencyHK, hk string, version *int64, origin audit.Event) (Delisting, Change, error) {
	return Cancel(ctx, s.DB, currencyHK, hk, version, origin)
}

// Override is Override on s.DB.
func (s Store) Override(ctx context.Context, currencyHK string, k currencies.Capability, allowed *bool, note string, origin audit.Event) (*models.CurrencyLifecycle, Change, error) {
	return Override(ctx, s.DB, currencyHK, k, allowed, note, origin)
}
