package restrictions

import (
	"context"
	"database/sql"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
)

// Store is the restrictions of a database, as an object that the handlers
// hold through an interface (#48). Each method is the function of the same
// name.
type Store struct {
	DB *sql.DB
}

// Create is Create on s.DB.
func (s Store) Create(ctx context.Context, n New, origin audit.Event) (Created, error) {
	return Create(ctx, s.DB, n, origin)
}

// Lift is Lift on s.DB.
func (s Store) Lift(ctx context.Context, userHK, hk, liftedBy, note string, expected *int64, origin audit.Event) (Restriction, error) {
	return Lift(ctx, s.DB, userHK, hk, liftedBy, note, expected, origin)
}

// List is List on s.DB.
func (s Store) List(ctx context.Context, userHK string, activeOnly bool, limit int) ([]Restriction, error) {
	return List(ctx, s.DB, userHK, activeOnly, limit)
}

// Active is Active on s.DB.
func (s Store) Active(ctx context.Context, userHK string) ([]Restriction, error) {
	return Active(ctx, s.DB, userHK)
}

// SecurityHold is SecurityHold on s.DB.
func (s Store) SecurityHold(ctx context.Context, userHK string) (*time.Time, error) {
	return SecurityHold(ctx, s.DB, userHK)
}
