package balances

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/sealtest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type books struct {
	t      *testing.T
	db     *sql.DB
	ledger ledger.Ledger
	store  Store
}

func newBooks(t *testing.T, mode string) books {
	t.Helper()
	db := pgtest.Migrated(t, "testdb_balances")
	db.SetMaxOpenConns(20)
	sealtest.Use(t, db, mode)
	return books{t: t, db: db, ledger: ledger.Ledger{DB: db}, store: Store{DB: db}}
}

func (b books) user() string {
	b.t.Helper()
	var hk string
	require.NoError(b.t, b.db.QueryRow(`INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk::text`).Scan(&hk))
	return hk
}

func (b books) currency(symbol string) string {
	b.t.Helper()
	var hk string
	require.NoError(b.t, b.db.QueryRow(`INSERT INTO currencies (status, info)
		VALUES ('approved', jsonb_build_object('symbol', $1::text, 'en_name', $1::text, 'fa_name', $1::text,
			'if_fiat', false, 'is_token', false, 'contract', '')) RETURNING hk::text`, symbol).Scan(&hk))
	return hk
}

func (b books) sub(user, label string) string {
	b.t.Helper()
	var id string
	require.NoError(b.t, b.db.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, $2) RETURNING id::text`, user, label).Scan(&id))
	return id
}

func mustUUID(s string) shared.UUID {
	id, err := shared.UUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return id
}

func account(user, sub string) ledger.Account {
	a := ledger.Account{UserHK: mustUUID(user)}
	if sub != "" {
		id := mustUUID(sub)
		a.SubAccountID = &id
	}
	return a
}

func amount(currency, value string) ledger.Amount {
	return ledger.Amount{Currency: mustUUID(currency), Value: shared.MustNewDecimalFromString(value)}
}

func (b books) fund(a ledger.Account, currency, value string) {
	b.t.Helper()
	ctx := context.Background()
	key := "fund:" + uuid.NewString()
	_, err := b.ledger.DetectDeposit(ctx, key+":detect", a, amount(currency, value), ledger.Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
	_, err = b.ledger.CreditDeposit(ctx, key+":credit", a, amount(currency, value), ledger.Ref{Type: "deposit", ID: key})
	require.NoError(b.t, err)
}

func (b books) resolve(user, raw string) (Account, error) {
	return b.store.Resolve(context.Background(), user, raw)
}

// A user sees their own balances, per account, and never another user's
// (#365): another user's sub-account, an unknown one, and a deleted one
// answer alike.
func TestBalances_OwnAccountsOnly(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t, rowseal.ModeEnforce)
	alice, bob := b.user(), b.user()
	usdt, btc := b.currency("USDT"), b.currency("BTC")
	trading, gone, bobs := b.sub(alice, "trading"), b.sub(alice, "old"), b.sub(bob, "bob")
	b.fund(account(alice, ""), usdt, "100")
	b.fund(account(alice, ""), btc, "0.5")
	b.fund(account(alice, trading), usdt, "30")
	b.fund(account(alice, gone), usdt, "1")
	b.fund(account(bob, ""), usdt, "999")
	b.fund(account(bob, bobs), usdt, "77")
	_, err := b.ledger.Lock(ctx, "w:lock", account(alice, ""), amount(usdt, "4.5"), ledger.Ref{Type: ledger.RefWithdrawal, ID: "w"})
	require.NoError(t, err)
	_, err = b.db.Exec(`UPDATE sub_accounts SET deleted_at = NOW() WHERE id = $1`, gone)
	require.NoError(t, err)

	main, err := b.resolve(alice, "")
	require.NoError(t, err)
	list, err := b.store.Balances(ctx, alice, main, "")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "BTC", list[0].Symbol)
	usdtMain := list[1]
	assert.Equal(t, "USDT", usdtMain.Symbol)
	assert.Equal(t, "95.5", usdtMain.Available.String())
	assert.Equal(t, "4.5", usdtMain.Locked.String())
	assert.Equal(t, "100", usdtMain.Total.String())
	assert.Equal(t, "0", usdtMain.PendingDeposit.String())
	assert.Nil(t, usdtMain.SubAccountID)
	assert.Positive(t, usdtMain.Version)

	sub, err := b.resolve(alice, trading)
	require.NoError(t, err)
	list, err = b.store.Balances(ctx, alice, sub, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "30", list[0].Available.String())
	assert.Equal(t, trading, *list[0].SubAccountID)

	all, err := b.resolve(alice, AccountAll)
	require.NoError(t, err)
	list, err = b.store.Balances(ctx, alice, all, "")
	require.NoError(t, err)
	assert.Len(t, list, 3, "the main account and the trading one; not the deleted one, and none of Bob's")
	list, err = b.store.Balances(ctx, alice, all, usdt)
	require.NoError(t, err)
	assert.Len(t, list, 2, "by currency")

	for name, raw := range map[string]string{"Bob's": bobs, "unknown": uuid.NewString(), "deleted": gone, "not an ID": "savings"} {
		_, err := b.resolve(alice, raw)
		assert.ErrorIs(t, err, ErrAccountNotFound, name)
	}
}

// A balance changed in SQL is not returned, and its finding is recorded
// (#365, #139). Balances are sealed from their first row (#404), so this
// holds in report mode too: report mode adopts only rows written before
// their seals, which a balance never is.
func TestBalances_ABalanceChangedInSQL(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{rowseal.ModeEnforce, rowseal.ModeReport} {
		t.Run(mode, func(t *testing.T) {
			b := newBooks(t, mode)
			alice, usdt := b.user(), b.currency("USDT")
			b.fund(account(alice, ""), usdt, "100")
			_, err := b.db.Exec(`UPDATE balances SET available = available + 1000000 WHERE user_id = $1`, alice)
			require.NoError(t, err)

			list, err := b.store.Balances(ctx, alice, Account{}, "")
			var unverified *Unverified
			require.ErrorAs(t, err, &unverified)
			assert.Equal(t, usdt, unverified.CurrencyHK)
			assert.ErrorIs(t, err, rowseal.ErrUnverified)
			assert.Nil(t, list)
			var findings int
			require.NoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'balances'`).Scan(&findings))
			assert.Equal(t, 1, findings)
		})
	}
}

// The history shows each change with its entry, kind, reference, and the
// balance after it, and its filters (#365).
func TestHistory_ShowsEachChange(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t, rowseal.ModeEnforce)
	alice, bob := b.user(), b.user()
	usdt, btc := b.currency("USDT"), b.currency("BTC")
	trading := b.sub(alice, "trading")
	b.fund(account(alice, ""), usdt, "100")
	b.fund(account(bob, ""), usdt, "50")
	_, err := b.ledger.Lock(ctx, "w:lock", account(alice, ""), amount(usdt, "10"), ledger.Ref{Type: ledger.RefWithdrawal, ID: "w1"})
	require.NoError(t, err)
	_, err = b.ledger.Transfer(ctx, "t1", account(alice, ""), account(alice, trading), amount(usdt, "20"), ledger.Ref{})
	require.NoError(t, err)
	b.fund(account(alice, ""), btc, "1")

	history := func(f Filter) []Change {
		t.Helper()
		list, err := b.store.History(ctx, alice, f, nil, 100)
		require.NoError(t, err)
		return list
	}
	main := history(Filter{})
	// Newest first, entry by entry; the changes of one entry share its
	// time, in no given order.
	kinds := []string{}
	for i, ch := range main {
		if i == 0 || main[i-1].EntryID != ch.EntryID {
			kinds = append(kinds, ch.Kind)
		}
	}
	assert.Equal(t, []string{"deposit_credited", "deposit_detected", "transfer", "lock", "deposit_credited", "deposit_detected"},
		kinds, "the main account only, none of Bob's")
	require.Len(t, main, 7, "the transfer has two changes of the main account")
	assert.ElementsMatch(t, []string{"LOCK", "TRANSFER_OUT"}, []string{main[2].ChangeType, main[3].ChangeType})
	assert.Equal(t, main[2].Time, main[3].Time)

	lock := main[4]
	assert.Equal(t, "USDT", lock.Symbol)
	assert.Equal(t, "10", lock.Amount.String())
	assert.Equal(t, "90", lock.Available.String(), "the balance after the change")
	assert.Equal(t, "10", lock.Locked.String())
	assert.Equal(t, &Reference{Type: ledger.RefWithdrawal, ID: "w1"}, lock.Reference)
	assert.Nil(t, lock.SubAccountID)
	assert.NotEmpty(t, lock.EntryID)

	assert.Len(t, history(Filter{Account: Account{All: true}}), 8, "and the transfer in of the trading account")
	in := history(Filter{Account: Account{Sub: ptr(uuid.MustParse(trading))}})
	require.Len(t, in, 1)
	assert.Equal(t, "TRANSFER_IN", in[0].ChangeType)
	assert.Equal(t, trading, *in[0].SubAccountID)
	assert.Len(t, history(Filter{CurrencyHK: btc}), 2)
	assert.Len(t, history(Filter{Kind: ledger.KindLock}), 1)
	assert.Empty(t, history(Filter{Until: main[len(main)-1].Time}), "until is exclusive")
	assert.Len(t, history(Filter{Since: main[0].Time}), 1, "since is inclusive")
	assert.Len(t, history(Filter{Since: main[1].Time}), 2)
}

func ptr[T any](v T) *T { return &v }

// The history pages through every change, with no gaps and no
// duplicates, while new changes are written (#365): the changes that came
// before the first page are all served once, and the new ones, newer than
// the first page, never in a later one.
func TestHistory_PagesWhileChangesAreWritten(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t, rowseal.ModeEnforce)
	alice, usdt := b.user(), b.currency("USDT")
	b.fund(account(alice, ""), usdt, "1000")
	for i := range 40 {
		_, err := b.ledger.Lock(ctx, fmt.Sprintf("lock:%d", i), account(alice, ""), amount(usdt, "1"), ledger.Ref{Type: "order", ID: fmt.Sprint(i)})
		require.NoError(t, err)
	}
	before, err := b.store.History(ctx, alice, Filter{}, nil, 1000)
	require.NoError(t, err)
	require.Len(t, before, 42)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes int
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				writes = i
				return
			default:
			}
			_, err := b.ledger.Release(ctx, fmt.Sprintf("release:%d", i), account(alice, ""), amount(usdt, "1"),
				ledger.Ref{Type: "order", ID: fmt.Sprint(i % 40)})
			if err != nil && !errors.Is(err, ledger.ErrInsufficientLocked) {
				assert.NoError(t, err)
			}
		}
	}()

	seen := map[string]bool{}
	var after *Position
	first := true
	var newest time.Time
	for {
		page, err := b.store.History(ctx, alice, Filter{}, after, 5)
		require.NoError(t, err)
		if first && len(page) > 0 {
			newest, first = page[0].Time, false
		}
		for _, ch := range page {
			assert.False(t, seen[ch.ID], "no duplicate")
			seen[ch.ID] = true
		}
		if len(page) < 5 {
			break
		}
		last := page[len(page)-1]
		after = &Position{Time: last.Time, ID: last.ID}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	for _, ch := range before {
		assert.True(t, seen[ch.ID], "no gap: %s %s", ch.Kind, ch.ID)
	}
	assert.Positive(t, writes, "changes were written meanwhile")
	for id := range seen {
		found := false
		for _, ch := range before {
			found = found || ch.ID == id
		}
		if !found {
			t.Logf("a newer change served on the first page: %s", id)
		}
	}
	assert.False(t, newest.IsZero())
}

// Each combination of the filters of the history is a range scan of an
// index that matches its order (#365), as EXPLAIN shows with sequential
// scans off, which a table of a test is too small to need.
func TestHistory_EachFilterUsesAnIndex(t *testing.T) {
	ctx := context.Background()
	b := newBooks(t, rowseal.ModeEnforce)
	alice, usdt := b.user(), b.currency("USDT")
	trading := b.sub(alice, "trading")
	b.fund(account(alice, ""), usdt, "10")

	conn, err := b.db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `SET enable_seqscan = off`)
	require.NoError(t, err)
	explained := Store{DB: explainer{conn}}
	sub := uuid.MustParse(trading)
	at := time.Now()
	for _, f := range []Filter{
		{}, {Account: Account{All: true}}, {Account: Account{Sub: &sub}},
		{CurrencyHK: usdt}, {Kind: ledger.KindLock}, {Since: at.Add(-time.Hour), Until: at},
		{Account: Account{All: true}, CurrencyHK: usdt, Kind: ledger.KindDepositCredited, Since: at.Add(-time.Hour), Until: at},
	} {
		for _, after := range []*Position{nil, {Time: at, ID: uuid.NewString()}} {
			plan, err := explained.History(ctx, alice, f, after, 50)
			_ = plan
			var e explainError
			require.ErrorAs(t, err, &e)
			assert.Regexp(t, `balance_changes_user_(currency_)?history`, e.plan, "%+v after %v", f, after != nil)
			if f.CurrencyHK != "" {
				assert.Contains(t, e.plan, "balance_changes_user_currency_history", "%+v", f)
			}
		}
	}
}

// explainer runs EXPLAIN of each query instead of the query, and returns
// the plan as an error, which History passes on.
type explainer struct{ conn *sql.Conn }

type explainError struct{ plan string }

func (e explainError) Error() string { return e.plan }

func (x explainer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := x.conn.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		plan += line + "\n"
	}
	return nil, explainError{plan}
}

func (x explainer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return x.conn.QueryRowContext(ctx, query, args...)
}
