package destinations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// The backoff of a pending ownership check (#158): a minute after the
// provider first failed, doubling to an hour.
const (
	FirstOwnershipRetry = time.Minute
	LastOwnershipRetry  = time.Hour
)

// ownershipBackoff is the wait after attempts failures.
func ownershipBackoff(attempts int) time.Duration {
	wait := FirstOwnershipRetry
	for i := 1; i < attempts && wait < LastOwnershipRetry; i++ {
		wait *= 2
	}
	return min(wait, LastOwnershipRetry)
}

// systemOrigin is the origin of what the system does on a request, such as
// a level that a matched card grants: the system actor, with the request's
// trace.
func systemOrigin(e audit.Event) audit.Event {
	return audit.Event{ActorType: audit.ActorSystem, RequestID: e.RequestID, IP: e.IP, UserAgent: e.UserAgent}
}

// CheckResult counts what a pass of CheckPending did.
type CheckResult struct {
	Matched, NotOwned, StillPending int
}

// CheckPending asks the provider again about the cards and IBANs that were
// added pending (#158), at most limit of them, the most overdue first. Each
// is asked outside any transaction; then, in one, the user's row is locked
// first, the destination after, and the destination is checked to be still
// pending and to match its seal (#139) before anything changes:
//
//   - matched: it is verified and usable, and the user's level may rise to
//     1 (verification.GrantIdentityLevel);
//   - not owned: it is removed, never having been usable;
//   - the provider still cannot answer: it waits longer, from a minute to
//     an hour.
//
// Two instances may ask about the same destination; the first to commit
// decides, and the other finds it no longer pending.
func (s Store) CheckPending(ctx context.Context, limit int) (CheckResult, error) {
	var result CheckResult
	if s.Ownership == nil {
		return result, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id::text, user_hk::text FROM withdrawal_destinations
		WHERE ownership_pending_since IS NOT NULL AND removed_at IS NULL AND ownership_next_check <= NOW()
		ORDER BY ownership_next_check LIMIT $1`, limit)
	if err != nil {
		return result, fmt.Errorf("list the pending destinations: %w", err)
	}
	type due struct{ id, user string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.user); err != nil {
			rows.Close()
			return result, fmt.Errorf("list the pending destinations: %w", err)
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	for _, d := range list {
		outcome, err := s.checkPendingOne(ctx, d.user, d.id)
		if err != nil {
			// One destination does not stop the others; the next pass
			// takes it again.
			log.Printf("The pending ownership check of destination %s failed: %v", d.id, err)
			continue
		}
		switch outcome {
		case outcomeMatched:
			result.Matched++
		case outcomeNotOwned:
			result.NotOwned++
		case outcomePending:
			result.StillPending++
		}
	}
	return result, nil
}

type checkOutcome int

const (
	outcomeNone checkOutcome = iota
	outcomeMatched
	outcomeNotOwned
	outcomePending
)

func (s Store) checkPendingOne(ctx context.Context, userHK, id string) (checkOutcome, error) {
	r, err := s.resolve(ctx, s.DB, userHK, id, false)
	if err != nil {
		return outcomeNone, err
	}
	if !r.OwnershipPending {
		return outcomeNone, nil
	}
	person, has, err := identity.PersonOf(ctx, s.DB, s.NationalIDs, userHK)
	if err != nil {
		return outcomeNone, err
	}
	timeout := s.OwnershipTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var match identity.Match
	var askErr error
	if has && !person.BirthDate.IsZero() {
		askCtx, cancel := context.WithTimeout(ctx, timeout)
		match, askErr = s.Ownership.MatchBank(askCtx, person, identity.BankAccount{Kind: string(r.Kind), Number: r.Value, Bank: r.Bank})
		cancel()
	} else {
		// The national ID was removed or replaced meanwhile: nothing can
		// match, and nothing is approved.
		askErr = identity.ErrUnavailable
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return outcomeNone, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return outcomeNone, fmt.Errorf("lock the user: %w", err)
	}
	row := rowseal.WithdrawalDestinations.NewRow()
	var attempts int
	var pending, removed bool
	err = tx.QueryRowContext(ctx, `SELECT ownership_pending_since IS NOT NULL, removed_at IS NOT NULL, ownership_attempts, `+
		rowseal.WithdrawalDestinations.Fields("")+`
		FROM withdrawal_destinations WHERE id = $1 AND user_hk = $2 FOR UPDATE`, id, userHK).
		Scan(append([]any{&pending, &removed, &attempts}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return outcomeNone, nil
	}
	if err != nil {
		return outcomeNone, fmt.Errorf("lock the destination: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.WithdrawalDestinations, row); err != nil {
		return outcomeNone, err
	}
	if !pending || removed {
		return outcomeNone, nil
	}
	origin := audit.Event{ActorType: audit.ActorSystem}
	event := audit.Of(origin, audit.ActionDestinationOwnershipCheck, audit.TargetWithdrawalDestination, id)
	provider := s.Ownership.Provider()
	switch {
	case askErr != nil && !errors.Is(askErr, identity.ErrUnsupported):
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_destinations SET ownership_attempts = ownership_attempts + 1,
				ownership_next_check = NOW() + make_interval(secs => $2) WHERE id = $1`,
			id, ownershipBackoff(attempts+1).Seconds()); err != nil {
			return outcomeNone, fmt.Errorf("delay the check: %w", err)
		}
		return outcomePending, tx.Commit()
	case askErr != nil, !match.Matched:
		// Not owned, or a kind that the provider cannot check: removed,
		// never having been usable.
		if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_destinations SET removed_at = NOW() WHERE id = $1`, id); err != nil {
			return outcomeNone, fmt.Errorf("remove the destination: %w", err)
		}
		event.Outcome, event.Reason = audit.Failure, "not_owned"
		event.Changes = &audit.Changes{After: map[string]any{"masked": r.Masked, "ownership_provider": provider, "ownership_ref": match.Ref}}
		if errors.Is(askErr, identity.ErrUnsupported) {
			event.Reason = "unsupported"
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalDestinations, "id = $1", id); err != nil {
			return outcomeNone, err
		}
		if err := audit.Record(ctx, tx, event); err != nil {
			return outcomeNone, err
		}
		return outcomeNotOwned, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE withdrawal_destinations SET verified_by = $2, verification_ref = $3,
			ownership_pending_since = NULL, ownership_next_check = NULL WHERE id = $1`, id, provider, match.Ref); err != nil {
		return outcomeNone, fmt.Errorf("verify the destination: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.WithdrawalDestinations, "id = $1", id); err != nil {
		return outcomeNone, err
	}
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{After: map[string]any{"masked": r.Masked, "ownership_provider": provider, "ownership_ref": match.Ref}}
	if err := audit.Record(ctx, tx, event); err != nil {
		return outcomeNone, err
	}
	if _, err := verification.GrantIdentityLevel(ctx, tx, userHK, origin); err != nil {
		return outcomeNone, err
	}
	return outcomeMatched, tx.Commit()
}
