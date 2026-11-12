// Package sealtest turns row seals on in tests, with throwaway data keys,
// as the server turns them on: for the rows (rowseal.Use), for the audit
// events (audit.UseSealer), and for the personal fields (field.Use).
package sealtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/require"
)

// Use seals the rows and the audit events written to db in mode, and
// encrypts the personal fields, until the test ends, with data keys of a
// new local KEK. db must have no data keys of another KEK, as a database
// that pgtest.Migrated emptied.
func Use(t testing.TB, db *sql.DB, mode string) *rowseal.Sealer {
	t.Helper()
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: db, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	s := rowseal.New(db, live, mode)
	t.Cleanup(rowseal.Use(s))
	t.Cleanup(audit.UseSealer(s))
	// The server encrypts the personal fields with the same keys (#140).
	t.Cleanup(field.Use(field.New(live)))
	return s
}
