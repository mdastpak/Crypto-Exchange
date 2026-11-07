package restrictions_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_restrictions"

var ctx = context.Background()

func origin(admin string) audit.Event {
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: admin}
}

func insertCurrency(t *testing.T, db *sql.DB, symbol string) string {
	t.Helper()
	info, err := json.Marshal(models.CurrencyInfo{EnName: symbol, FaName: symbol, Symbol: symbol})
	require.NoError(t, err)
	var hk string
	require.NoError(t, db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved', $1::jsonb) RETURNING hk`, info).Scan(&hk))
	return hk
}

func create(t *testing.T, db *sql.DB, n restrictions.New) restrictions.Created {
	t.Helper()
	if n.CreatedBy == "" {
		n.CreatedBy = uuid.NewString()
	}
	if n.Reason == "" {
		n.Reason = restrictions.ReasonAMLReview
	}
	created, err := restrictions.Create(ctx, db, n, origin(n.CreatedBy))
	require.NoError(t, err)
	return created
}

func refusal(t *testing.T, err error) *restrictions.Refusal {
	t.Helper()
	var r *restrictions.Refusal
	require.True(t, errors.As(err, &r), "want a refusal, got %v", err)
	return r
}

func TestCheck_BlocksOnlyWhatTheRestrictionsName(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "check@example.com"})
	btc, eth := insertCurrency(t, db, "BTC"), insertCurrency(t, db, "ETH")

	require.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, btc), "no restriction")

	ends := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpWithdraw}, Reason: restrictions.ReasonUserRequest, EndsAt: &ends})
	create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeAsset, CurrencyHK: eth,
		Operations: []restrictions.Operation{restrictions.OpTrade, restrictions.OpConvert}})

	r := refusal(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, btc))
	assert.Equal(t, restrictions.CodeOperationRestricted, r.Code)
	require.NotNil(t, r.Until, "a user request may show its end")
	assert.True(t, ends.Equal(*r.Until))
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, btc))
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpBuy, btc), "the hold is of ETH only")

	r = refusal(t, restrictions.Check(ctx, db, user, restrictions.OpSell, eth))
	assert.Equal(t, restrictions.CodeAssetRestricted, r.Code, "trade covers sell")
	assert.Equal(t, eth, r.CurrencyHK)
	assert.Nil(t, r.Until, "an AML review never shows its end")
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, eth))
	assert.Equal(t, restrictions.CodeOperationRestricted, refusal(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, eth)).Code,
		"a block of every currency comes before a hold")

	other := usertest.Insert(t, db, usertest.User{Email: "other@example.com"})
	assert.NoError(t, restrictions.Check(ctx, db, other, restrictions.OpWithdraw, btc), "restrictions are per user")
	assert.Error(t, restrictions.Check(ctx, db, user, "fly", ""))
}

func TestCheck_LiftedAndEndedRestrictionsDoNotApply(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "ended@example.com"})
	admin := uuid.NewString()
	lifted := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpDeposit}, CreatedBy: admin}).Restriction
	ended := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
		Operations: []restrictions.Operation{restrictions.OpWithdraw}}).Restriction
	assert.Equal(t, restrictions.CodeOperationRestricted, refusal(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, "")).Code)

	r, err := restrictions.Lift(ctx, db, user, lifted.HK, admin, "cleared", nil, origin(admin))
	require.NoError(t, err)
	assert.False(t, r.Active)
	assert.Equal(t, int64(2), r.Version)
	_, err = db.Exec(`UPDATE user_restrictions SET created_at = NOW() - INTERVAL '2 hours', ends_at = NOW() - INTERVAL '1 hour' WHERE hk = $1`, ended.HK)
	require.NoError(t, err)

	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, ""))
	assert.NoError(t, restrictions.Check(ctx, db, user, restrictions.OpWithdraw, ""), "an end needs no job")
	active, err := restrictions.Active(ctx, db, user)
	require.NoError(t, err)
	assert.Empty(t, active)
	all, err := restrictions.List(ctx, db, user, false, 10)
	require.NoError(t, err)
	assert.Len(t, all, 2, "the history stays")
}

func TestCreate_FreezeEndsEverySessionInItsTransaction(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "frozen@example.com"})
	for i := 0; i < 2; i++ {
		_, err := db.Exec(`INSERT INTO sessions (user_id, auth_method, access_token_hash, refresh_token_hash, expires_at)
			VALUES ($1, 'OTP', 'a', 'r', NOW() + INTERVAL '1 hour')`, user)
		require.NoError(t, err)
	}
	admin := uuid.NewString()
	created := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeAccount,
		Reason: restrictions.ReasonCourtOrder, CaseRef: "CO-1", CreatedBy: admin})
	assert.Equal(t, 2, created.EndedSessions)
	assert.Empty(t, created.Restriction.Operations)
	assert.True(t, created.Restriction.Active)

	var active int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = $1 AND status = 'ACTIVE'`, user).Scan(&active))
	assert.Zero(t, active)
	var revokes, adds int
	require.NoError(t, db.QueryRow(`SELECT count(*) FILTER (WHERE action = $1 AND reason = 'account_frozen'),
		count(*) FILTER (WHERE action = $2 AND reason = 'court_order' AND target_id = $3)
		FROM audit_events WHERE actor_id = $4`, audit.ActionUserSessionRevoke, audit.ActionUserRestrictionAdd, user, admin).Scan(&revokes, &adds))
	assert.Equal(t, 2, revokes)
	assert.Equal(t, 1, adds)

	assert.Equal(t, restrictions.CodeAccountFrozen, refusal(t, restrictions.Check(ctx, db, user, restrictions.OpDeposit, "")).Code)
	var frozen bool
	require.NoError(t, db.QueryRow(`SELECT `+restrictions.FrozenSQL("users.hk")+` FROM users WHERE hk = $1`, user).Scan(&frozen))
	assert.True(t, frozen)
}

func TestCreate_Refusals(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	admin := usertest.Insert(t, db, usertest.User{Role: "admin", Email: "admin@example.com"})
	deleted := usertest.Insert(t, db, usertest.User{Deleted: true, Email: "gone@example.com"})
	user := usertest.Insert(t, db, usertest.User{Email: "user@example.com"})
	freeze := restrictions.New{Scope: restrictions.ScopeAccount, Reason: restrictions.ReasonFraud, CreatedBy: uuid.NewString()}

	for name, tc := range map[string]struct {
		n    restrictions.New
		want error
	}{
		"an administrator": {restrictions.New{UserHK: admin}, restrictions.ErrNotUser},
		"a deleted user":   {restrictions.New{UserHK: deleted}, restrictions.ErrNotUser},
		"no such user":     {restrictions.New{UserHK: uuid.NewString()}, restrictions.ErrUserNotFound},
		"no such currency": {restrictions.New{UserHK: user, Scope: restrictions.ScopeAsset, CurrencyHK: uuid.NewString(), Operations: []restrictions.Operation{restrictions.OpWithdraw}}, restrictions.ErrCurrencyNotFound},
	} {
		n := freeze
		n.UserHK = tc.n.UserHK
		if tc.n.Scope != "" {
			n.Scope, n.CurrencyHK, n.Operations = tc.n.Scope, tc.n.CurrencyHK, tc.n.Operations
		}
		_, err := restrictions.Create(ctx, db, n, origin(n.CreatedBy))
		assert.ErrorIs(t, err, tc.want, name)
	}
}

func TestLift_Refusals(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "lift@example.com"})
	other := usertest.Insert(t, db, usertest.User{Email: "lift-other@example.com"})
	admin := uuid.NewString()
	r := create(t, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeAccount, CreatedBy: admin}).Restriction

	_, err := restrictions.Lift(ctx, db, other, r.HK, admin, "x", nil, origin(admin))
	assert.ErrorIs(t, err, restrictions.ErrNotFound, "a restriction of another user")
	stale := int64(7)
	_, err = restrictions.Lift(ctx, db, user, r.HK, admin, "x", &stale, origin(admin))
	var conflict *restrictions.VersionConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, int64(1), conflict.Current)
	current := int64(1)
	_, err = restrictions.Lift(ctx, db, user, r.HK, admin, "x", &current, origin(admin))
	require.NoError(t, err)
	_, err = restrictions.Lift(ctx, db, user, r.HK, admin, "x", nil, origin(admin))
	assert.ErrorIs(t, err, restrictions.ErrAlreadyLifted)
}

// TestCreate_WaitsForOperationsInFlight checks the order of a restriction
// and an operation on two instances: an operation that holds Lock finishes
// before the restriction commits, and one that starts after sees it.
func TestCreate_WaitsForOperationsInFlight(t *testing.T) {
	db := pgtest.Migrated(t, testDBName)
	user := usertest.Insert(t, db, usertest.User{Email: "inflight@example.com"})

	op, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer op.Rollback()
	require.NoError(t, restrictions.Lock(ctx, op, user))
	require.NoError(t, restrictions.Check(ctx, op, user, restrictions.OpWithdraw, ""))

	done := make(chan error, 1)
	go func() {
		_, err := restrictions.Create(ctx, db, restrictions.New{UserHK: user, Scope: restrictions.ScopeOperation,
			Operations: []restrictions.Operation{restrictions.OpWithdraw}, Reason: restrictions.ReasonAMLReview, CreatedBy: uuid.NewString()},
			origin(uuid.NewString()))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the restriction did not wait for the operation in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, op.Commit())
	require.NoError(t, <-done)

	next, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer next.Rollback()
	require.NoError(t, restrictions.Lock(ctx, next, user))
	assert.Equal(t, restrictions.CodeOperationRestricted, refusal(t, restrictions.Check(ctx, next, user, restrictions.OpWithdraw, "")).Code)
	assert.ErrorIs(t, restrictions.Lock(ctx, next, uuid.NewString()), restrictions.ErrUserNotFound)
}

func TestPublic_HidesWhatTheUserMustNotSee(t *testing.T) {
	ends := time.Now().Add(time.Hour)
	currency := uuid.NewString()
	r := restrictions.Restriction{HK: "id", Scope: restrictions.ScopeAsset, CurrencyHK: &currency,
		Operations: []restrictions.Operation{restrictions.OpWithdraw}, Reason: restrictions.ReasonSanctions,
		CaseRef: "S-1", Note: "secret", EndsAt: &ends}
	p := restrictions.Public(r)
	assert.Nil(t, p.Until)
	assert.Equal(t, &currency, p.CurrencyHK)
	r.Reason = restrictions.ReasonSecurity
	assert.Equal(t, &ends, restrictions.Public(r).Until)
	assert.True(t, restrictions.ReasonCourtOrder.Confidential())
	assert.False(t, restrictions.ReasonUserRequest.Confidential())
}
