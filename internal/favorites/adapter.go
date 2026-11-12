package favorites

import (
	"context"
	"database/sql"
)

// Store is the favorites of a database, as an object that the handlers hold
// through an interface (#48). Each method is the function of the same name.
type Store struct {
	DB *sql.DB
}

// Get is Get on s.DB.
func (s Store) Get(ctx context.Context, userHK string) (List, error) { return Get(ctx, s.DB, userHK) }

// Add is Add on s.DB.
func (s Store) Add(ctx context.Context, userHK string, kind Kind, target string, max int) (bool, error) {
	return Add(ctx, s.DB, userHK, kind, target, max)
}

// Remove is Remove on s.DB.
func (s Store) Remove(ctx context.Context, userHK string, kind Kind, target string) (bool, error) {
	return Remove(ctx, s.DB, userHK, kind, target)
}

// Reorder is Reorder on s.DB.
func (s Store) Reorder(ctx context.Context, userHK string, kind Kind, ordered []string) error {
	return Reorder(ctx, s.DB, userHK, kind, ordered)
}
