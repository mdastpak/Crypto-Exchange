package platform

import (
	"context"
	"database/sql"

	"github.com/mdastpak/crypto-exchange/internal/audit"
)

// Store is the platform controls of a database, as an object that the
// handlers hold through an interface (#48). Each method is the function of
// the same name.
type Store struct {
	DB *sql.DB
}

// Open is Open on s.DB.
func (s Store) Open(ctx context.Context) ([]*Control, error) {
	return Open(ctx, s.DB)
}

// List is List on s.DB.
func (s Store) List(ctx context.Context, limit int) ([]*Control, error) {
	return List(ctx, s.DB, limit)
}

// Create is Create on s.DB.
func (s Store) Create(ctx context.Context, n New, origin audit.Event) (*Control, error) {
	return Create(ctx, s.DB, n, origin)
}

// Update is Update on s.DB.
func (s Store) Update(ctx context.Context, hk string, changes Changes, expected *int64, origin audit.Event) (*Control, error) {
	return Update(ctx, s.DB, hk, changes, expected, origin)
}

// Lift is Lift on s.DB.
func (s Store) Lift(ctx context.Context, r LiftRequest, origin audit.Event) (LiftResult, error) {
	return Lift(ctx, s.DB, r, origin)
}
