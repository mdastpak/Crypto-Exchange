package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/mdastpak/crypto-exchange/internal/domain/balance"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Holdings reports what custody holds of a currency, as its wallets and
// providers say. It returns false when it does not know, as for a currency
// that custody does not hold yet.
type Holdings interface {
	Holdings(ctx context.Context, currency shared.UUID) (shared.Decimal, bool, error)
}

// CurrencyReport is the books of one currency.
type CurrencyReport struct {
	Currency shared.UUID
	Symbol   string
	// Users is what users own (available + locked), and Pending what is
	// detected for them and not credited yet.
	Users, Pending shared.Decimal
	// Accounts are the balances of the exchange's accounts, by name.
	Accounts map[string]shared.Decimal
	// Imbalance is the sum of the positions of users and of the exchange's
	// accounts, which is zero unless a row was changed outside the ledger.
	Imbalance shared.Decimal
	// Expected is what custody must hold: the opposite of the custody
	// account. Actual is what it reports holding, and Difference
	// Actual - Expected; both are nil when Holdings does not know.
	Expected           shared.Decimal
	Actual, Difference *shared.Decimal
}

// Mismatch is a balance whose stored values differ from the replay of its
// changes, or whose changes do not follow each other.
type Mismatch struct {
	BalanceID shared.UUID
	Reason    string
}

// Report is the outcome of Reconcile.
type Report struct {
	Currencies []CurrencyReport
	// Balances is the number of balances replayed, and Mismatches those
	// whose replay differs.
	Balances   int
	Mismatches []Mismatch
}

// OK reports books without a problem: every balance is the replay of its
// changes, every currency balances, and custody holds what it must, where
// Holdings knows.
func (r Report) OK() bool {
	if len(r.Mismatches) > 0 {
		return false
	}
	for _, c := range r.Currencies {
		if !c.Imbalance.IsZero() || (c.Difference != nil && !c.Difference.IsZero()) {
			return false
		}
	}
	return true
}

// replayBatchSize is how many balances Reconcile replays at a time.
const replayBatchSize = 500

// Reconcile checks the books, reading only:
//
//   - every balance is replayed from zero through its changes, each of
//     which must start where the one before ended, carry the next version,
//     and be the transition its type and amount describe; the result must
//     be the stored balance;
//   - in every currency, the positions of users and the accounts of the
//     exchange must sum to zero;
//   - what custody must hold is compared with what holdings reports, when
//     holdings is set and knows the currency.
//
// It reads in one REPEATABLE READ transaction, one snapshot of the books,
// so that the operations that go on meanwhile never show as a difference.
func (l Ledger) Reconcile(ctx context.Context, holdings Holdings) (Report, error) {
	tx, err := l.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	return reconcile(ctx, tx, holdings)
}

// querier reads the books: a transaction, for one snapshot of them.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reconcile is Reconcile in q.
func reconcile(ctx context.Context, q querier, holdings Holdings) (Report, error) {
	var report Report
	if err := replay(ctx, q, &report); err != nil {
		return Report{}, err
	}
	currencies, err := sums(ctx, q)
	if err != nil {
		return Report{}, err
	}
	for _, c := range currencies {
		if holdings != nil {
			actual, known, err := holdings.Holdings(ctx, c.Currency)
			if err != nil {
				return Report{}, fmt.Errorf("the holdings of %s: %w", c.Symbol, err)
			}
			if known {
				difference := actual.Sub(c.Expected)
				c.Actual, c.Difference = &actual, &difference
			}
		}
		report.Currencies = append(report.Currencies, c)
	}
	return report, nil
}

// replay replays every balance, in batches by ID.
func replay(ctx context.Context, q querier, report *Report) error {
	after := ""
	for {
		rows, err := q.QueryContext(ctx, `SELECT id::text, available, locked, pending_deposit, version FROM balances
			WHERE $1 = '' OR id > $1::uuid ORDER BY id LIMIT $2`, after, replayBatchSize)
		if err != nil {
			return fmt.Errorf("read the balances: %w", err)
		}
		stored := make(map[string]balance.Balance)
		var ids []string
		for rows.Next() {
			var id string
			var b balance.Balance
			if err := rows.Scan(&id, &b.Available, &b.Locked, &b.PendingDeposit, &b.Version); err != nil {
				rows.Close()
				return fmt.Errorf("read the balances: %w", err)
			}
			stored[id] = b
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the balances: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		if err := replayBatch(ctx, q, ids, stored, report); err != nil {
			return err
		}
		report.Balances += len(ids)
		if len(ids) < replayBatchSize {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

// replayBatch replays the changes of the balances ids.
func replayBatch(ctx context.Context, q querier, ids []string, stored map[string]balance.Balance, report *Report) error {
	rows, err := q.QueryContext(ctx, `SELECT balance_id::text, version, change_type, amount,
			old_available, new_available, old_locked, new_locked, old_pending, new_pending
		FROM balance_changes WHERE balance_id = ANY($1::uuid[]) ORDER BY balance_id, version`, ids)
	if err != nil {
		return fmt.Errorf("read the changes: %w", err)
	}
	defer rows.Close()
	// The replayed state of each balance, from zero at version 1, and the
	// first problem found in its changes.
	replayed := make(map[string]*balance.Balance, len(ids))
	problems := make(map[string]string)
	for _, id := range ids {
		replayed[id] = &balance.Balance{Version: 1}
	}
	for rows.Next() {
		var id, change string
		var version int
		var amount, oldAvailable, newAvailable, oldLocked, newLocked, oldPending, newPending shared.Decimal
		if err := rows.Scan(&id, &version, &change, &amount, &oldAvailable, &newAvailable, &oldLocked, &newLocked,
			&oldPending, &newPending); err != nil {
			return fmt.Errorf("read the changes: %w", err)
		}
		if problems[id] != "" {
			continue
		}
		b := replayed[id]
		switch {
		case version != b.Version+1:
			problems[id] = fmt.Sprintf("change %d follows version %d", version, b.Version)
			continue
		case !oldAvailable.Equal(b.Available) || !oldLocked.Equal(b.Locked) || !oldPending.Equal(b.PendingDeposit):
			problems[id] = fmt.Sprintf("change %d does not start where version %d ended", version, b.Version)
			continue
		}
		event, err := b.Apply(balance.BalanceChangeType(change), amount, balance.Reference{})
		if err != nil {
			problems[id] = fmt.Sprintf("change %d (%s of %s) is not a valid transition: %v", version, change, amount, err)
			continue
		}
		if !event.NewAvailable.Equal(newAvailable) || !event.NewLocked.Equal(newLocked) || !event.NewPending.Equal(newPending) {
			problems[id] = fmt.Sprintf("change %d ends elsewhere than %s of %s leads", version, change, amount)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the changes: %w", err)
	}
	for _, id := range ids {
		reason := problems[id]
		if reason == "" {
			b, s := replayed[id], stored[id]
			if b.Version != s.Version || !b.Available.Equal(s.Available) || !b.Locked.Equal(s.Locked) || !b.PendingDeposit.Equal(s.PendingDeposit) {
				reason = fmt.Sprintf("stored at version %d (%s, %s, %s), replayed to version %d (%s, %s, %s)",
					s.Version, s.Available, s.Locked, s.PendingDeposit, b.Version, b.Available, b.Locked, b.PendingDeposit)
			}
		}
		if reason != "" {
			var balanceID shared.UUID
			if err := balanceID.Scan(id); err != nil {
				return err
			}
			report.Mismatches = append(report.Mismatches, Mismatch{BalanceID: balanceID, Reason: reason})
		}
	}
	return nil
}

// sums totals the balances of users and the accounts of the exchange by
// currency.
func sums(ctx context.Context, q querier) ([]CurrencyReport, error) {
	byCurrency := make(map[string]*CurrencyReport)
	get := func(hk shared.UUID) *CurrencyReport {
		c, ok := byCurrency[hk.String()]
		if !ok {
			c = &CurrencyReport{Currency: hk, Accounts: make(map[string]shared.Decimal)}
			for _, a := range Accounts {
				c.Accounts[a] = shared.Decimal{}
			}
			byCurrency[hk.String()] = c
		}
		return c
	}
	rows, err := q.QueryContext(ctx, `SELECT currency_hk, COALESCE(SUM(available::numeric + locked::numeric), 0),
		COALESCE(SUM(pending_deposit::numeric), 0) FROM balances GROUP BY currency_hk`)
	if err != nil {
		return nil, fmt.Errorf("sum the balances: %w", err)
	}
	for rows.Next() {
		var hk shared.UUID
		var users, pending shared.Decimal
		if err := rows.Scan(&hk, &users, &pending); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sum the balances: %w", err)
		}
		c := get(hk)
		c.Users, c.Pending = users, pending
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sum the balances: %w", err)
	}
	rows, err = q.QueryContext(ctx, `SELECT currency_hk, account, SUM(amount::numeric) FROM ledger_postings GROUP BY currency_hk, account`)
	if err != nil {
		return nil, fmt.Errorf("sum the postings: %w", err)
	}
	for rows.Next() {
		var hk shared.UUID
		var account string
		var sum shared.Decimal
		if err := rows.Scan(&hk, &account, &sum); err != nil {
			rows.Close()
			return nil, fmt.Errorf("sum the postings: %w", err)
		}
		get(hk).Accounts[account] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sum the postings: %w", err)
	}

	out := make([]CurrencyReport, 0, len(byCurrency))
	for _, c := range byCurrency {
		total := c.Users.Add(c.Pending)
		for _, a := range Accounts {
			total = total.Add(c.Accounts[a])
		}
		c.Imbalance = total
		c.Expected = c.Accounts[AccountCustody].Neg()
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(info->>'symbol', '') FROM currencies WHERE hk = $1`, c.Currency).
			Scan(&c.Symbol); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read the currency: %w", err)
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}
