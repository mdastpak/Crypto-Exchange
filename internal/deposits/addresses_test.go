package deposits

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/tron"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// The xpub of BIP32 test vector 1 at m/0H stands for the account.
const xpub = "xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw"

type fixture struct {
	Service
	t    *testing.T
	usdt string
	trx  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	db := pgtest.Migrated(t, "testdb_deposits")
	sealtest.Use(t, db, rowseal.ModeEnforce)
	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE currency_networks SET deposit_enabled = true WHERE network = 'tron'`)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, db, rowseal.CurrencyNetworks, "network = 'tron'"))
	policy, err := verification.NewPolicy(config.VerificationConfig{MinLevel: map[string]int{"deposit_crypto": 0}})
	require.NoError(t, err)
	x, err := tron.ParseXPub(xpub)
	require.NoError(t, err)
	f := fixture{Service: Service{DB: db, Networks: map[string]Deriver{"tron": x}, Levels: verification.Store{DB: db, Policy: policy}}, t: t}
	require.NoError(t, db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&f.usdt))
	require.NoError(t, db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'TRX'`).Scan(&f.trx))
	return f
}

func (f fixture) user(email string) string {
	f.t.Helper()
	hk := usertest.Insert(f.t, f.DB, usertest.User{Email: email})
	require.NoError(f.t, rowseal.Reseal(context.Background(), f.DB, rowseal.Users, "hk = $1", hk))
	return hk
}

// Each user gets one address per network, which every currency of the
// network shares, derived from the xpub by the next index (#68).
func TestGet_OneAddressPerUserAndNetwork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice, bob := f.user("alice@example.com"), f.user("bob@example.com")

	a, err := f.Get(ctx, alice, f.usdt, "tron", audit.Event{})
	require.NoError(t, err)
	x, _ := tron.ParseXPub(xpub)
	want, _, _ := x.Derive(0)
	assert.Equal(t, want, a.Address, "index 0, derived from the xpub")
	again, err := f.Get(ctx, alice, f.trx, "tron", audit.Event{})
	require.NoError(t, err)
	assert.Equal(t, a.Address, again.Address, "TRX and USDT share the address")
	b, err := f.Get(ctx, bob, f.usdt, "tron", audit.Event{})
	require.NoError(t, err)
	want1, _, _ := x.Derive(1)
	assert.Equal(t, want1, b.Address)

	owner, ok, err := Owner(ctx, f.DB, "tron", b.Address)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, bob, owner)
	_, ok, err = Owner(ctx, f.DB, "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	require.NoError(t, err)
	assert.False(t, ok)

	list, err := f.List(ctx, alice)
	require.NoError(t, err)
	require.Len(t, list, 1)
	var events int
	require.NoError(t, f.DB.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'user.deposit_address'`).Scan(&events))
	assert.Equal(t, 2, events, "once per address")
}

func TestGet_Refusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user("alice@example.com")

	_, err := f.Get(ctx, alice, f.usdt, "ethereum", audit.Event{})
	assert.ErrorIs(t, err, ErrUnsupported, "a network without a deriver")
	_, err = f.Get(ctx, alice, "not-a-uuid", "tron", audit.Event{})
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = f.DB.Exec(`UPDATE currency_networks SET deposit_enabled = false WHERE network = 'tron' AND currency_hk = $1`, f.usdt)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, f.DB, rowseal.CurrencyNetworks, "network = 'tron'"))
	_, err = f.Get(ctx, alice, f.usdt, "tron", audit.Event{})
	assert.ErrorIs(t, err, ErrClosed)

	_, err = restrictions.Create(ctx, f.DB, restrictions.New{UserHK: alice, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpDeposit}, Reason: restrictions.ReasonAMLReview,
		CreatedBy: "00000000-0000-0000-0000-000000000000"}, audit.Event{ActorType: audit.ActorSystem})
	require.NoError(t, err)
	var refusal *restrictions.Refusal
	_, err = f.Get(ctx, alice, f.trx, "tron", audit.Event{})
	assert.ErrorAs(t, err, &refusal, "a user whose deposits are restricted")
}

// An address swapped in SQL would credit another user's deposits: it is
// refused with its finding (#68).
func TestAddress_ChangedInSQL_Refused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	alice := f.user("alice@example.com")
	a, err := f.Get(ctx, alice, f.usdt, "tron", audit.Event{})
	require.NoError(t, err)
	_, err = f.DB.Exec(`UPDATE deposit_addresses SET address = 'TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t'`)
	require.NoError(t, err)
	_, _, err = Owner(ctx, f.DB, "tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	_, err = f.Get(ctx, alice, f.usdt, "tron", audit.Event{})
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.NotEmpty(t, a.Address)
}
