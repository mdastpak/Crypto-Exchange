package rowseal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMAC_AnOptionalColumnKeepsTheOlderSeals adds an optional column to a
// table (#337). A row whose new column is NULL has the seal it had before
// the column existed, so the rows sealed before still match; a value in the
// column changes the seal, so it cannot be added, nor taken back to NULL,
// unnoticed.
func TestMAC_AnOptionalColumnKeepsTheOlderSeals(t *testing.T) {
	db := pgtest.Migrated(t, "testdb_rowseal")
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: db, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	key, err := live.Current(keys.PurposeRowMAC)
	require.NoError(t, err)

	before := &Table{Name: "t", Key: "id", Columns: []Column{text("a"), text("b")}}
	after := &Table{Name: "t", Key: "id", Columns: []Column{text("a"), text("b"), optional(text("c"))}}
	value := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	sealed, err := mac(key, before, &Row{Key: "1", Version: 3, Values: []sql.NullString{value("x"), {}}})
	require.NoError(t, err)

	same, err := mac(key, after, &Row{Key: "1", Version: 3, Values: []sql.NullString{value("x"), {}, {}}})
	require.NoError(t, err)
	assert.Equal(t, sealed, same, "a NULL optional column is left out")

	set, err := mac(key, after, &Row{Key: "1", Version: 3, Values: []sql.NullString{value("x"), {}, value("")}})
	require.NoError(t, err)
	assert.NotEqual(t, sealed, set, "a value, even empty, is sealed")

	// A required column is never left out: NULL and absent differ.
	required := &Table{Name: "t", Key: "id", Columns: []Column{text("a"), text("b"), text("c")}}
	withNull, err := mac(key, required, &Row{Key: "1", Version: 3, Values: []sql.NullString{value("x"), {}, {}}})
	require.NoError(t, err)
	assert.NotEqual(t, sealed, withNull)
}
