package withdrawals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// PayoutBook opens the destination of a withdrawal for its payouts:
// destinations.Store.
type PayoutBook interface {
	Resolve(ctx context.Context, q destinations.Querier, userHK, id string) (destinations.Resolved, error)
}

// Payouts pays the withdrawals (#70). Every instance runs it: each claims
// the rows it works on with FOR UPDATE SKIP LOCKED, so that a withdrawal is
// planned once and a payout sent once.
//
//   - A queued withdrawal is planned: split into payouts, a crypto one into
//     one on chain, a rial one across the bank rails that reach its
//     destination, within their limits and the destination's daily limits
//     (Plan). The plan is recorded before anything is sent.
//   - A planned payout is sent when its day comes and its rail is open. It
//     is marked submitted before it reaches the provider, so that one whose
//     result is unknown is never sent again: Status settles it.
//   - A completed payout is a withdrawal in the ledger, for its amount and
//     fee. A failed one goes to the next rail, or its amount and fee are
//     released alone; the payouts paid stay paid.
type Payouts struct {
	DB           *sql.DB
	Ledger       ledger.Ledger
	Destinations PayoutBook
	// Provider pays every payout that Chains does not: the manual one.
	Provider Provider
	// Chains pays the payouts on chain of a network, by the network, such
	// as the TRON signer (#68).
	Chains   map[string]Provider
	Settings config.PayoutsConfig
	// Location is the zone of the rails' hours and days.
	Location *time.Location
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// providerFor is the provider that pays a part on rail of a withdrawal on
// network.
func (p Payouts) providerFor(rail, network string) Provider {
	if c, ok := p.Chains[network]; ok && rail == RailChain {
		return c
	}
	return p.Provider
}

// byName is the provider that a payout was planned with; false for one
// that this instance does not have, which it leaves to another.
func (p Payouts) byName(name string) (Provider, bool) {
	if p.Provider != nil && p.Provider.Name() == name {
		return p.Provider, true
	}
	for _, c := range p.Chains {
		if c.Name() == name {
			return c, true
		}
	}
	return nil, false
}

// batch bounds the rows of each step of a pass.
const batch = 50

func (p Payouts) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p Payouts) location() *time.Location {
	if p.Location != nil {
		return p.Location
	}
	return time.UTC
}

// dayStart is the start of the day of t in the zone of the rails, plus
// days.
func (p Payouts) dayStart(t time.Time, days int) time.Time {
	l := t.In(p.location())
	return time.Date(l.Year(), l.Month(), l.Day()+days, 0, 0, 0, 0, p.location())
}

// Run plans the queued withdrawals, sends the payouts that are due, and
// checks those sent, once. It returns how many rows it moved.
func (p Payouts) Run(ctx context.Context) (int, error) {
	planned, err := p.planQueued(ctx)
	if err != nil {
		return planned, err
	}
	sent, err := p.submitDue(ctx)
	if err != nil {
		return planned + sent, err
	}
	checked, err := p.checkSubmitted(ctx)
	return planned + sent + checked, err
}

// ids returns the ids that query selects, at most batch, without locking
// them: each is claimed on its own, so that one that cannot move holds up
// no other.
func (p Payouts) ids(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := p.DB.QueryContext(ctx, query+` LIMIT `+fmt.Sprint(batch), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (p Payouts) planQueued(ctx context.Context) (int, error) {
	ids, err := p.ids(ctx, `SELECT id FROM withdrawals WHERE status = 'queued' ORDER BY created_at`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		done, err := p.plan(ctx, id)
		switch {
		case errors.Is(err, ErrUnplannable), errors.Is(err, rowseal.ErrUnverified):
			// It stays queued, and the user may cancel it, until the rails or
			// its finding allow it.
			log.Printf("Withdrawal %s cannot be planned yet: %v", id, err)
		case err != nil:
			return n, err
		case done:
			n++
		}
	}
	return n, nil
}

// plan splits the queued withdrawal id into its payouts.
func (p Payouts) plan(ctx context.Context, id string) (bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var claimed string
	err = tx.QueryRowContext(ctx, `SELECT id FROM withdrawals WHERE id = $1 AND status = 'queued' FOR UPDATE SKIP LOCKED`, id).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w, err := read(ctx, tx, `w.id = $1`, id)
	if err != nil {
		return false, err
	}
	d, err := p.Destinations.Resolve(ctx, tx, w.UserHK, w.DestinationID)
	if err != nil {
		return false, err
	}
	n, err := currencies.NetworkOf(ctx, tx, w.CurrencyHK, w.Network, false)
	if err != nil {
		return false, err
	}
	now := p.now()
	parts := []Part{{Rail: RailChain, Amount: w.Amount}}
	if d.Fiat() {
		used, err := p.used(ctx, tx, w.DestinationID, p.dayStart(now, 0))
		if err != nil {
			return false, err
		}
		if parts, err = Plan(w.Amount, RailsFor(d.Destination, p.Settings), used); err != nil {
			return false, err
		}
	}
	if err := p.insertParts(ctx, tx, w, parts, w.Fee, n.Decimals, 1, now); err != nil {
		return false, err
	}
	if err := setStatus(ctx, tx, w.ID, StatusProcessing, "", false); err != nil {
		return false, err
	}
	e := audit.Of(audit.Event{ActorType: audit.ActorSystem}, audit.ActionWithdrawalPayoutsPlanned, audit.TargetWithdrawal, w.ID)
	e.Changes = &audit.Changes{After: map[string]any{"payouts": len(parts), "status": string(StatusProcessing)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// insertParts records parts as payouts of w from seq on, with fee split
// across them.
func (p Payouts) insertParts(ctx context.Context, tx *sql.Tx, w Withdrawal, parts []Part, fee shared.Decimal, decimals int, seq int, now time.Time) error {
	amounts := make([]shared.Decimal, len(parts))
	for i, part := range parts {
		amounts[i] = part.Amount
	}
	fees := SplitFee(fee, amounts, int32(decimals)) //nolint:gosec // G115: decimals are from 0 to 36.
	for i, part := range parts {
		when := now
		if part.Day > 0 {
			when = p.dayStart(now, part.Day)
		}
		n := seq + i
		if _, err := tx.ExecContext(ctx, `INSERT INTO withdrawal_payouts (withdrawal_id, seq, rail, provider, amount, fee, status,
				scheduled_for, idempotency_key)
			VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, 'planned', $7, $8)`,
			w.ID, n, part.Rail, p.providerFor(part.Rail, w.Network).Name(), part.Amount.String(), fees[i].String(), when, fmt.Sprintf("%s-%d", w.ID, n)); err != nil {
			return fmt.Errorf("record a payout: %w", err)
		}
	}
	return rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "withdrawal_id = $1 AND seq >= $2", w.ID, seq)
}

// used returns what the destination receives on each rail on each day
// from today, by the payouts planned, sent, and paid.
func (p Payouts) used(ctx context.Context, tx *sql.Tx, destinationID string, today time.Time) (Used, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.rail, FLOOR(EXTRACT(EPOCH FROM (p.scheduled_for - $2)) / 86400)::int, SUM(p.amount)::text
		FROM withdrawal_payouts p JOIN withdrawals w ON w.id = p.withdrawal_id
		WHERE w.destination_id = $1 AND p.status IN ('planned', 'submitted', 'completed')
			AND p.scheduled_for >= $2 AND p.scheduled_for < $2 + make_interval(days => $3)
		GROUP BY 1, 2`, destinationID, today, PlanDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := Used{}
	for rows.Next() {
		var rail, sum string
		var day int
		if err := rows.Scan(&rail, &day, &sum); err != nil {
			return nil, err
		}
		d, err := shared.NewDecimalFromString(sum)
		if err != nil {
			return nil, err
		}
		used.add(day, rail, d)
	}
	return used, rows.Err()
}

func (p Payouts) submitDue(ctx context.Context) (int, error) {
	ids, err := p.ids(ctx, `SELECT id FROM withdrawal_payouts WHERE status = 'planned' AND scheduled_for <= $1
		AND provider = ANY(string_to_array($2, ',')) ORDER BY scheduled_for, seq`, p.now(), strings.Join(p.names(), ","))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		sent, err := p.submit(ctx, id)
		if errors.Is(err, rowseal.ErrUnverified) {
			log.Printf("Payout %s fails its seal and is not sent: %v", id, err)
			continue
		}
		if err != nil {
			return n, err
		}
		if sent {
			n++
		}
	}
	return n, nil
}

// submit sends the planned payout id, when its rail is open.
func (p Payouts) submit(ctx context.Context, id string) (bool, error) {
	pay, ok, err := p.claim(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	provider, known := p.byName(pay.Provider)
	t, err := p.transfer(ctx, pay)
	if err == nil && !known {
		err = fmt.Errorf("the provider %s of the payout is not configured", pay.Provider)
	}
	if err != nil {
		// Nothing reached the provider: the payout waits for the next pass.
		if undo := p.resend(ctx, id); undo != nil {
			return true, fmt.Errorf("%w (and putting the payout back: %v)", err, undo)
		}
		return false, err
	}
	ref, err := provider.Submit(ctx, t)
	tx, txErr := p.DB.BeginTx(ctx, nil)
	if txErr != nil {
		return true, txErr
	}
	defer tx.Rollback()
	current, readErr := readPayout(ctx, tx, `p.id = $1`, true, id)
	if readErr != nil {
		return true, readErr
	}
	if current.Status != PayoutSubmitted {
		return true, nil
	}
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET provider_ref = NULLIF($2, ''), last_error = NULL WHERE id = $1`, id, ref); err != nil {
			return true, err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "id = $1", id); err != nil {
			return true, err
		}
	case errors.Is(err, ErrTransferRefused):
		if err := p.fail(ctx, tx, current, err.Error(), audit.Event{ActorType: audit.ActorSystem}); err != nil {
			return true, err
		}
	default:
		// The result is unknown: Status settles it before anything is sent
		// again.
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET last_error = $2 WHERE id = $1`, id, shorten(err.Error())); err != nil {
			return true, err
		}
	}
	return true, tx.Commit()
}

// claim marks the planned payout id submitted, before it reaches the
// provider. A payout whose rail is closed now waits for its opening.
func (p Payouts) claim(ctx context.Context, id string) (Payout, bool, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, false, err
	}
	defer tx.Rollback()
	var claimed string
	err = tx.QueryRowContext(ctx, `SELECT id FROM withdrawal_payouts WHERE id = $1 AND status = 'planned' FOR UPDATE SKIP LOCKED`, id).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return Payout{}, false, nil
	}
	if err != nil {
		return Payout{}, false, err
	}
	pay, err := readPayout(ctx, tx, `p.id = $1`, false, id)
	if err != nil {
		return Payout{}, false, err
	}
	if opens, open := p.opening(pay.Rail, p.now()); !open {
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET scheduled_for = $2 WHERE id = $1`, id, opens); err != nil {
			return Payout{}, false, err
		}
		return Payout{}, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET status = 'submitted', attempts = attempts + 1, submitted_at = NOW()
		WHERE id = $1`, id); err != nil {
		return Payout{}, false, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "id = $1", id); err != nil {
		return Payout{}, false, err
	}
	pay.Status = PayoutSubmitted
	return pay, true, tx.Commit()
}

// opening reports whether rail is open at t, and if not, when it opens
// next.
func (p Payouts) opening(rail string, t time.Time) (time.Time, bool) {
	c, ok := p.Settings.Rails[rail]
	if !ok {
		return t, true
	}
	open, closing, hasHours := c.Window()
	if !hasHours {
		return t, true
	}
	l := t.In(p.location())
	minute := l.Hour()*60 + l.Minute()
	if minute >= open && minute < closing {
		return t, true
	}
	day := p.dayStart(t, 0)
	if minute >= closing {
		day = p.dayStart(t, 1)
	}
	return day.Add(time.Duration(open) * time.Minute), false
}

// transfer is the payout as its provider receives it, with its
// destination in the clear.
func (p Payouts) transfer(ctx context.Context, pay Payout) (Transfer, error) {
	d, err := p.Destinations.Resolve(ctx, p.DB, pay.UserHK, pay.DestinationKey)
	if err != nil {
		return Transfer{}, err
	}
	t := Transfer{PayoutID: pay.ID, IdempotencyKey: pay.idempotencyKey, Ref: pay.providerRef, Rail: pay.Rail,
		Currency: pay.Currency, Amount: pay.Amount, Destination: d}
	if pay.Rail == RailChain {
		// The asset by its contract, which a rename of the currency keeps,
		// and its precision, from the registry, checked against its seal.
		n, err := currencies.NetworkOf(ctx, p.DB, pay.currencyHK, pay.Network, false)
		if err != nil {
			return Transfer{}, err
		}
		t.Network, t.Contract, t.Decimals = pay.Network, n.ContractAddress, n.Decimals
	}
	return t, nil
}

// names are the providers of this instance.
func (p Payouts) names() []string {
	out := []string{}
	if p.Provider != nil {
		out = append(out, p.Provider.Name())
	}
	for _, c := range p.Chains {
		out = append(out, c.Name())
	}
	return out
}

func (p Payouts) checkSubmitted(ctx context.Context) (int, error) {
	// An administrator records each payout of the manual provider.
	var automatic []string
	for _, name := range p.names() {
		if name != config.PayoutProviderManual {
			automatic = append(automatic, name)
		}
	}
	if len(automatic) == 0 {
		return 0, nil
	}
	ids, err := p.ids(ctx, `SELECT id FROM withdrawal_payouts WHERE status = 'submitted' AND provider = ANY(string_to_array($1, ',')) ORDER BY submitted_at`,
		strings.Join(automatic, ","))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		pay, err := readPayout(ctx, p.DB, `p.id = $1`, false, id)
		if err != nil {
			continue
		}
		provider, known := p.byName(pay.Provider)
		if !known {
			continue
		}
		t, err := p.transfer(ctx, pay)
		if err != nil {
			continue
		}
		state, err := provider.Status(ctx, t)
		if err != nil {
			log.Printf("Failed to read the state of payout %s from %s: %v", id, pay.Provider, err)
			continue
		}
		origin := audit.Event{ActorType: audit.ActorSystem}
		switch state.Status {
		case TransferCompleted:
			_, err = p.Complete(ctx, id, state.TrackingID, origin)
		case TransferFailed:
			_, err = p.Fail(ctx, id, state.Reason, origin)
		case TransferNotReceived:
			err = p.resend(ctx, id)
		default:
			continue
		}
		if err != nil && !errors.Is(err, ErrPayoutNotOpen) {
			return n, err
		}
		n++
	}
	return n, nil
}

// resend puts back a submitted payout that its provider never got, so
// that it is sent again with the same key.
func (p Payouts) resend(ctx context.Context, id string) error {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pay, err := readPayout(ctx, tx, `p.id = $1`, true, id)
	if err != nil {
		return err
	}
	if pay.Status != PayoutSubmitted {
		return ErrPayoutNotOpen
	}
	if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET status = 'planned', submitted_at = NULL WHERE id = $1`, id); err != nil {
		return err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "id = $1", id); err != nil {
		return err
	}
	return tx.Commit()
}

// MaxTrackingID bounds a tracking number, and a reason of a failure.
const MaxTrackingID = 200

// Complete records the payout id as paid, with the bank's tracking number
// or the hash on chain: its amount and fee leave the user's balance, in the
// same transaction. An administrator, or the provider's report, completes
// it.
func (p Payouts) Complete(ctx context.Context, id, trackingID string, origin audit.Event) (Payout, error) {
	trackingID = strings.TrimSpace(trackingID)
	if trackingID == "" || utf8.RuneCountInString(trackingID) > MaxTrackingID {
		return Payout{}, invalid("tracking_id", "is required, at most %d characters", MaxTrackingID)
	}
	return p.settlePayout(ctx, id, func(tx *sql.Tx, w Withdrawal, pay Payout) error {
		if pay.Status != PayoutSubmitted {
			return ErrPayoutNotOpen
		}
		if byHand(origin, pay) {
			return ErrPayoutAutomatic
		}
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET status = 'completed', tracking_id = $2, completed_at = NOW(),
				last_error = NULL WHERE id = $1`, id, trackingID); err != nil {
			return err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "id = $1", id); err != nil {
			return err
		}
		if _, err := p.Ledger.In(tx).Withdraw(ctx, "payout:"+id, account(w.UserHK),
			ledger.Amount{Currency: uuidOf(w.CurrencyHK), Value: pay.Amount}, pay.Fee,
			ledger.Ref{Type: ledger.RefWithdrawal, ID: w.ID}); err != nil {
			return err
		}
		e := audit.Of(origin, audit.ActionWithdrawalPayoutComplete, audit.TargetWithdrawal, w.ID)
		e.Changes = &audit.Changes{After: map[string]any{"payout": id, "seq": pay.Seq, "rail": pay.Rail,
			"amount": pay.Amount.String(), "fee": pay.Fee.String(), "tracking_id": trackingID}}
		return audit.Record(ctx, tx, e)
	})
}

// Fail records the payout id as failed, with reason: its amount goes to the
// next rail that reaches the destination, or, when none can take it, its
// amount and fee are released to the user. The payouts paid stay paid.
func (p Payouts) Fail(ctx context.Context, id, reason string, origin audit.Event) (Payout, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxTrackingID {
		return Payout{}, invalid("reason", "is required, at most %d characters", MaxTrackingID)
	}
	return p.settlePayout(ctx, id, func(tx *sql.Tx, _ Withdrawal, pay Payout) error {
		if byHand(origin, pay) {
			return ErrPayoutAutomatic
		}
		return p.fail(ctx, tx, pay, reason, origin)
	})
}

// byHand reports an administrator who records a payout that a provider
// pays by itself: its provider's report settles it, as one recorded by
// hand could be paid twice, or released while it is paid.
func byHand(origin audit.Event, pay Payout) bool {
	return origin.ActorType == audit.ActorAdmin && pay.Provider != config.PayoutProviderManual
}

// settlePayout runs change on the payout id and its withdrawal, both
// locked, and then settles the withdrawal's status, in one transaction.
func (p Payouts) settlePayout(ctx context.Context, id string, change func(*sql.Tx, Withdrawal, Payout) error) (Payout, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Payout{}, ErrPayoutNotFound
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, err
	}
	defer tx.Rollback()
	var withdrawalID string
	err = tx.QueryRowContext(ctx, `SELECT withdrawal_id FROM withdrawal_payouts WHERE id = $1`, id).Scan(&withdrawalID)
	if errors.Is(err, sql.ErrNoRows) {
		return Payout{}, ErrPayoutNotFound
	}
	if err != nil {
		return Payout{}, err
	}
	// The withdrawal first, as everything that changes it locks it.
	w, err := readLocked(ctx, tx, `w.id = $1`, true, withdrawalID)
	if err != nil {
		return Payout{}, err
	}
	pay, err := readPayout(ctx, tx, `p.id = $1`, true, id)
	if err != nil {
		return Payout{}, err
	}
	if err := change(tx, w, pay); err != nil {
		return Payout{}, err
	}
	if err := settleWithdrawal(ctx, tx, w.ID); err != nil {
		return Payout{}, err
	}
	pay, err = readPayout(ctx, tx, `p.id = $1`, false, id)
	if err != nil {
		return Payout{}, err
	}
	return pay, tx.Commit()
}

// fail fails pay in tx, and plans its amount again on the rails after
// its own, or releases it.
func (p Payouts) fail(ctx context.Context, tx *sql.Tx, pay Payout, reason string, origin audit.Event) error {
	if pay.Status != PayoutSubmitted && pay.Status != PayoutPlanned {
		return ErrPayoutNotOpen
	}
	w, err := read(ctx, tx, `w.id = $1`, pay.WithdrawalID)
	if err != nil {
		return err
	}
	replanned := 0
	if pay.Rail != RailChain {
		replanned, err = p.replan(ctx, tx, w, pay)
		if err != nil && !errors.Is(err, ErrUnplannable) {
			return err
		}
	}
	status := PayoutFailed
	if replanned == 0 {
		status = PayoutReleased
		if _, err := p.Ledger.In(tx).Release(ctx, "payout:"+pay.ID+":release", account(w.UserHK),
			ledger.Amount{Currency: uuidOf(w.CurrencyHK), Value: pay.Amount.Add(pay.Fee)},
			ledger.Ref{Type: ledger.RefWithdrawal, ID: w.ID}); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_payouts SET status = $2, failed_at = NOW(), last_error = $3 WHERE id = $1`,
		pay.ID, string(status), shorten(reason)); err != nil {
		return err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalPayouts, "id = $1", pay.ID); err != nil {
		return err
	}
	e := audit.Of(origin, audit.ActionWithdrawalPayoutFail, audit.TargetWithdrawal, w.ID)
	e.Changes = &audit.Changes{After: map[string]any{"payout": pay.ID, "seq": pay.Seq, "rail": pay.Rail,
		"amount": pay.Amount.String(), "status": string(status), "replanned": replanned, "reason": shorten(reason)}}
	return audit.Record(ctx, tx, e)
}

// replan plans the amount of the failed payout pay again, on the rails
// that reach its destination and have not failed for its withdrawal. It
// returns how many payouts it planned.
func (p Payouts) replan(ctx context.Context, tx *sql.Tx, w Withdrawal, pay Payout) (int, error) {
	d, err := p.Destinations.Resolve(ctx, tx, w.UserHK, w.DestinationID)
	if err != nil {
		return 0, err
	}
	failed := []string{pay.Rail}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT rail FROM withdrawal_payouts WHERE withdrawal_id = $1 AND status IN ('failed', 'released')`, w.ID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var rail string
		if err := rows.Scan(&rail); err != nil {
			rows.Close()
			return 0, err
		}
		failed = append(failed, rail)
	}
	rows.Close()
	rails := slices.DeleteFunc(RailsFor(d.Destination, p.Settings), func(r Rail) bool { return slices.Contains(failed, r.Name) })
	now := p.now()
	used, err := p.used(ctx, tx, w.DestinationID, p.dayStart(now, 0))
	if err != nil {
		return 0, err
	}
	parts, err := Plan(pay.Amount, rails, used)
	if err != nil {
		return 0, err
	}
	n, err := currencies.NetworkOf(ctx, tx, w.CurrencyHK, w.Network, false)
	if err != nil {
		return 0, err
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM withdrawal_payouts WHERE withdrawal_id = $1`, w.ID).Scan(&next); err != nil {
		return 0, err
	}
	if err := p.insertParts(ctx, tx, w, parts, pay.Fee, n.Decimals, next, now); err != nil {
		return 0, err
	}
	return len(parts), nil
}

// settleWithdrawal sets the status of the withdrawal id from its payouts:
// processing while none is paid, partially_paid while some are, and, once
// none is open, completed when every one was paid, failed when none was,
// and partially_paid otherwise.
func settleWithdrawal(ctx context.Context, tx *sql.Tx, id string) error {
	var open, completed, released int
	err := tx.QueryRowContext(ctx, `SELECT
			COUNT(*) FILTER (WHERE status IN ('planned', 'submitted')),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (WHERE status = 'released')
		FROM withdrawal_payouts WHERE withdrawal_id = $1`, id).Scan(&open, &completed, &released)
	if err != nil {
		return err
	}
	var status Status
	final, reason := false, ""
	switch {
	case open > 0 && completed > 0:
		status = StatusPartiallyPaid
	case open > 0:
		status = StatusProcessing
	case released == 0:
		status, final = StatusCompleted, true
	case completed == 0:
		status, final, reason = StatusFailed, true, "The withdrawal could not be paid; its funds were released."
	default:
		status, final, reason = StatusPartiallyPaid, true, "Part of the withdrawal could not be paid; that part was released."
	}
	return setStatus(ctx, tx, id, status, reason, final)
}

// setStatus sets the status of the withdrawal id, with reason, and its
// completion time when final.
func setStatus(ctx context.Context, tx *sql.Tx, id string, status Status, reason string, final bool) error {
	if _, err := tx.ExecContext(ctx, `UPDATE withdrawals SET status = $2, reason = COALESCE(NULLIF($3, ''), reason),
			completed_at = CASE WHEN $4 THEN NOW() ELSE completed_at END
		WHERE id = $1`, id, string(status), reason, final); err != nil {
		return err
	}
	return rowseal.Reseal(ctx, tx, rowseal.Withdrawals, "id = $1", id)
}

// shorten bounds a text of a provider for the database.
func shorten(s string) string {
	if utf8.RuneCountInString(s) <= MaxTrackingID {
		return s
	}
	return string([]rune(s)[:MaxTrackingID])
}
