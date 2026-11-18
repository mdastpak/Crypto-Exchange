// Package idcheck verifies a user's identity (#158): their national ID and
// date of birth at the national registry, through a KYC provider, and their
// mobile number against the national ID when the account has one.
//
// A check is a sealed row of identity_checks (#139). The user submits the
// national ID and the date of birth; the row is written pending, and the
// provider is asked outside any transaction. Its answer is applied in a
// transaction that locks the user's row first, then the check, which must
// still be pending and match its seal:
//
//   - matched: the national ID is stored, verified, with the date of birth
//     and a keyed blind index that keeps one account per person; a
//     national ID verified for another account makes the check taken;
//   - mismatched: final, and counted against the user's attempts;
//   - the provider cannot answer: the check stays pending, and RetryDue asks
//     again, from a minute to an hour later. Nothing is approved on a
//     failure.
//
// A verified national ID, with a card or an IBAN whose owner matched it,
// raises the user to level 1 (verification.GrantIdentityLevel). National
// IDs, dates of birth, and mobile numbers are never logged or put in
// errors.
package idcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The states of a user's identity, as Status reports them.
const (
	StateNone     = "none"
	StatePending  = "pending"
	StateVerified = "verified"
	StateRejected = "rejected"
)

// The statuses of a check.
const (
	checkPending    = "pending"
	checkMatched    = "matched"
	checkMismatched = "mismatched"
	checkTaken      = "taken"
)

// The errors of a submission.
var (
	// ErrAlreadyVerified is a user whose national ID is verified with its
	// date of birth: changing it is a matter for support.
	ErrAlreadyVerified = errors.New("the identity is verified already")
	// ErrPending is a user whose check waits for the provider.
	ErrPending = errors.New("a check of the identity is pending")
	// ErrTooManyAttempts is a user who failed MaxFailures checks in a day.
	ErrTooManyAttempts = errors.New("too many failed checks of the identity")
	// ErrUnavailable is a server without a provider of the check.
	ErrUnavailable = errors.New("the identity cannot be checked now")
	// ErrNoKeys is a server without the data-protection keys, which seal
	// the national ID and compute its blind index.
	ErrNoKeys = errors.New("the data-protection keys are not loaded")
	// ErrNationalIDTaken is a national ID verified for another account:
	// one person has one account (#158).
	ErrNationalIDTaken = identity.ErrNationalIDTaken
)

// Invalid lists the fields of a submission that are not valid.
type Invalid map[string]string

func (e Invalid) Error() string { return fmt.Sprintf("invalid identity: %v", map[string]string(e)) }

// MaxFailures is how many checks of a user may fail, mismatched or taken,
// in a day: so few that national IDs cannot be tried one by one.
const MaxFailures = 5

// The backoff of a pending check: a minute after the provider first
// failed, doubling to an hour.
const (
	FirstRetry = time.Minute
	LastRetry  = time.Hour
)

func backoff(attempts int) time.Duration {
	wait := FirstRetry
	for i := 1; i < attempts && wait < LastRetry; i++ {
		wait *= 2
	}
	return min(wait, LastRetry)
}

// Service checks identities.
type Service struct {
	DB *sql.DB
	// Inquiry checks a national ID and its date of birth. Nil is no
	// provider: submissions are refused with ErrUnavailable.
	Inquiry identity.IdentityInquiry
	// Mobile checks the user's mobile number against the national ID,
	// when the account has one. Nil skips it.
	Mobile identity.MobileOwnership
	// Sealer seals the national IDs and the dates of birth.
	Sealer identity.Sealer
	// Timeout bounds one question to the provider; zero is ten seconds.
	Timeout time.Duration
	now     func() time.Time
}

func (s Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Status is a user's identity as the user sees it.
type Status struct {
	State string `json:"state" enums:"none,pending,verified,rejected"`
	// NationalID is the national ID of a verified or pending identity,
	// masked.
	NationalID string `json:"national_id,omitempty" example:"00******79"`
	// Reason is why the last check was rejected: mismatched, when the
	// registry does not have that national ID with that date of birth or
	// the mobile number is not registered to it, or taken, when the
	// national ID is verified for another account.
	Reason     string     `json:"reason,omitempty" enums:"mismatched,taken"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// AttemptsLeft is how many more checks may fail today.
	AttemptsLeft int `json:"attempts_left"`
}

func mask(nationalID string) string {
	if len(nationalID) != 10 {
		return ""
	}
	return nationalID[:2] + "******" + nationalID[8:]
}

func checkAAD(id, column string) []byte { return []byte("identity_checks." + column + ":" + id) }

// Status returns the user's identity.
func (s Service) Status(ctx context.Context, userHK string) (Status, error) {
	failures, err := s.failures(ctx, userHK)
	if err != nil {
		return Status{}, err
	}
	st := Status{State: StateNone, AttemptsLeft: max(MaxFailures-failures, 0)}
	person, verified, err := identity.PersonOf(ctx, s.DB, s.Sealer, userHK)
	if err != nil {
		return Status{}, err
	}
	if verified && !person.BirthDate.IsZero() {
		var at time.Time
		if err := s.DB.QueryRowContext(ctx, `SELECT verified_at FROM user_national_ids WHERE user_hk = $1`, userHK).Scan(&at); err != nil {
			return Status{}, err
		}
		st.State, st.NationalID, st.VerifiedAt = StateVerified, mask(person.NationalID), &at
		return st, nil
	}
	var id, status string
	var sealed []byte
	row := rowseal.IdentityChecks.NewRow()
	err = s.DB.QueryRowContext(ctx, `SELECT id::text, status, national_id_sealed, `+rowseal.IdentityChecks.Fields("")+`
		FROM identity_checks WHERE user_hk = $1 ORDER BY created_at DESC LIMIT 1`, userHK).
		Scan(append([]any{&id, &status, &sealed}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read the check: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.IdentityChecks, row); err != nil {
		return Status{}, err
	}
	switch status {
	case checkPending:
		st.State = StatePending
	case checkMismatched, checkTaken:
		st.State, st.Reason = StateRejected, status
	default:
		return st, nil
	}
	if s.Sealer != nil {
		if plain, err := s.Sealer.OpenField(checkAAD(id, "national_id_sealed"), sealed); err == nil {
			st.NationalID = mask(string(plain))
		}
	}
	return st, nil
}

// failures counts the user's checks of the last day that failed.
func (s Service) failures(ctx context.Context, userHK string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_checks
		WHERE user_hk = $1 AND status IN ('mismatched', 'taken') AND created_at > NOW() - INTERVAL '1 day'`, userHK).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the failed checks: %w", err)
	}
	return n, nil
}

// Submit checks the national ID and the date of birth (YYYY-MM-DD) of the
// user, and records origin, the request's audit event. It returns the
// identity after the check: verified, rejected, or pending while the
// provider cannot answer.
func (s Service) Submit(ctx context.Context, userHK, nationalID, birthDate string, origin audit.Event) (Status, error) {
	problems := Invalid{}
	if !identity.ValidNationalID(nationalID) {
		problems["national_id"] = "is not a valid national ID"
	}
	birth, err := identity.ParseBirthDate(birthDate, s.clock())
	if err != nil {
		problems["birth_date"] = err.Error()
	}
	if len(problems) > 0 {
		return Status{}, problems
	}
	if s.Inquiry == nil {
		return Status{}, ErrUnavailable
	}
	cipher := field.Active()
	if cipher == nil || s.Sealer == nil {
		return Status{}, ErrNoKeys
	}
	current, verified, err := identity.PersonOf(ctx, s.DB, s.Sealer, userHK)
	if err != nil {
		return Status{}, err
	}
	// A national ID stored without a date of birth may be checked again
	// with one, but not replaced.
	if verified && (!current.BirthDate.IsZero() || current.NationalID != nationalID) {
		return Status{}, ErrAlreadyVerified
	}
	failures, err := s.failures(ctx, userHK)
	if err != nil {
		return Status{}, err
	}
	if failures >= MaxFailures {
		return Status{}, ErrTooManyAttempts
	}
	index, err := cipher.Index(identity.NationalIDIndexDomain, nationalID)
	if err != nil {
		return Status{}, err
	}

	id := uuid.NewString()
	sealedID, err := s.Sealer.SealField(checkAAD(id, "national_id_sealed"), []byte(nationalID))
	if err != nil {
		return Status{}, fmt.Errorf("seal the national ID: %w", err)
	}
	sealedBirth, err := s.Sealer.SealField(checkAAD(id, "birth_date_sealed"), []byte(birth.Format(time.DateOnly)))
	if err != nil {
		return Status{}, fmt.Errorf("seal the date of birth: %w", err)
	}
	if err := s.record(ctx, userHK, id, sealedID, index, sealedBirth); err != nil {
		return Status{}, err
	}
	person := identity.Person{NationalID: nationalID, BirthDate: birth}
	// A national ID verified for another account is refused before the
	// provider is asked.
	var taken bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_national_ids
		WHERE national_id_bidx = ANY($1::bytea[]) AND user_hk <> $2)`,
		cipher.Indexes(identity.NationalIDIndexDomain, nationalID), userHK).Scan(&taken); err != nil {
		return Status{}, fmt.Errorf("look for the national ID: %w", err)
	}
	var match identity.Match
	var askErr error
	if !taken {
		match, askErr = s.ask(ctx, userHK, person)
	}
	if err := s.settle(ctx, userHK, id, person, taken, match, askErr, origin); err != nil {
		return Status{}, err
	}
	st, err := s.Status(ctx, userHK)
	if err == nil && st.Reason == checkTaken {
		// One person, one account: the refusal is recorded, and answered
		// as one.
		return st, ErrNationalIDTaken
	}
	return st, err
}

// record writes a pending check, sealed. One check of a user is pending at
// a time.
func (s Service) record(ctx context.Context, userHK, id string, sealedID, index, sealedBirth []byte) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_checks (id, user_hk, national_id_sealed, national_id_bidx, birth_date_sealed,
			status, provider, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, 'pending', $6, NOW())`, id, userHK, sealedID, index, sealedBirth, s.Inquiry.Provider())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "identity_checks_user_pending" {
		return ErrPending
	}
	if err != nil {
		return fmt.Errorf("record the check: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.IdentityChecks, "id = $1", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ask asks the provider about the person, and about the user's verified
// mobile number when the account has one. It runs outside any
// transaction. A failure to answer wraps identity.ErrUnavailable.
func (s Service) ask(ctx context.Context, userHK string, person identity.Person) (identity.Match, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	match, err := s.Inquiry.Identify(askCtx, person)
	if err != nil || !match.Matched || s.Mobile == nil {
		return match, err
	}
	mobile, has, err := s.mobileOf(ctx, userHK)
	if err != nil {
		return identity.Match{}, fmt.Errorf("%w: read the mobile number: %w", identity.ErrUnavailable, err)
	}
	if !has {
		return match, nil
	}
	owned, err := s.Mobile.Match(askCtx, person.NationalID, mobile)
	if err != nil {
		return identity.Match{}, err
	}
	if !owned.Matched {
		return owned, nil
	}
	return match, nil
}

// mobileOf returns the user's verified mobile number, when they have one.
func (s Service) mobileOf(ctx context.Context, userHK string) (string, bool, error) {
	var id int64
	var plain *string
	var sealed []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id, value, value_enc FROM user_contacts
		WHERE user_hk = $1 AND type = 'mobile' AND removed_at IS NULL AND verified_at IS NOT NULL
		ORDER BY id LIMIT 1`, userHK).Scan(&id, &plain, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value, err := db.OpenContact(id, plain, sealed)
	return value, err == nil, err
}

// settle applies the answer to the pending check id: in a transaction that
// locks the user's row first, then the check, which must still be pending
// and match its seal.
func (s Service) settle(ctx context.Context, userHK, id string, person identity.Person, taken bool,
	match identity.Match, askErr error, origin audit.Event) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return fmt.Errorf("lock the user: %w", err)
	}
	var status string
	var attempts int
	row := rowseal.IdentityChecks.NewRow()
	err = tx.QueryRowContext(ctx, `SELECT status, attempts, `+rowseal.IdentityChecks.Fields("")+`
		FROM identity_checks WHERE id = $1 AND user_hk = $2 FOR UPDATE`, id, userHK).
		Scan(append([]any{&status, &attempts}, row.Dest()...)...)
	if err != nil {
		return fmt.Errorf("lock the check: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.IdentityChecks, row); err != nil {
		return err
	}
	if status != checkPending {
		return nil
	}
	event := audit.Of(origin, audit.ActionUserIdentityCheck, audit.TargetUser, userHK)
	after := map[string]any{"check": id, "provider": s.Inquiry.Provider()}
	if match.Ref != "" {
		after["provider_ref"] = match.Ref
	}
	switch {
	case !taken && askErr != nil:
		if !errors.Is(askErr, identity.ErrUnavailable) {
			log.Printf("The identity check %s failed: %v", id, askErr)
		}
		_, err := tx.ExecContext(ctx, `UPDATE identity_checks SET attempts = attempts + 1,
			next_attempt_at = NOW() + make_interval(secs => $2) WHERE id = $1`, id, backoff(attempts+1).Seconds())
		if err != nil {
			return fmt.Errorf("delay the check: %w", err)
		}
		return tx.Commit()
	case !taken && match.Matched:
		err := identity.Save(ctx, tx, s.Sealer, userHK, identity.Verified{Person: person, Provider: s.Inquiry.Provider(),
			Ref: match.Ref, At: s.clock()})
		switch {
		case errors.Is(err, identity.ErrNationalIDTaken):
			taken = true
		case err != nil:
			return err
		default:
			status = checkMatched
		}
	}
	switch {
	case taken:
		status = checkTaken
		event.Outcome, event.Reason = audit.Failure, checkTaken
	case status == checkMatched:
		event.Outcome = audit.Success
	default:
		status = checkMismatched
		event.Outcome, event.Reason = audit.Failure, checkMismatched
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identity_checks SET status = $2, provider_ref = NULLIF($3, ''),
			decided_at = NOW(), next_attempt_at = NULL, attempts = attempts + 1 WHERE id = $1`, id, status, match.Ref); err != nil {
		return fmt.Errorf("decide the check: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.IdentityChecks, "id = $1", id); err != nil {
		return err
	}
	after["status"] = status
	event.Changes = &audit.Changes{After: after}
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	if status == checkMatched {
		system := audit.Event{ActorType: audit.ActorSystem, RequestID: origin.RequestID, IP: origin.IP, UserAgent: origin.UserAgent}
		if _, err := verification.GrantIdentityLevel(ctx, tx, userHK, system); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RetryDue asks the provider again about the pending checks that are due,
// at most limit of them, the most overdue first. A check that fails again
// waits longer; each is applied as Submit applies its first answer.
func (s Service) RetryDue(ctx context.Context, limit int) (int, error) {
	if s.Inquiry == nil || s.Sealer == nil {
		return 0, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id::text, user_hk::text FROM identity_checks
		WHERE status = 'pending' AND next_attempt_at <= NOW() ORDER BY next_attempt_at LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("list the pending checks: %w", err)
	}
	type due struct{ id, user string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.user); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	done := 0
	for _, d := range list {
		if err := s.retry(ctx, d.user, d.id); err != nil {
			log.Printf("The retry of identity check %s failed: %v", d.id, err)
			continue
		}
		done++
	}
	return done, nil
}

func (s Service) retry(ctx context.Context, userHK, id string) error {
	var sealedID, sealedBirth []byte
	row := rowseal.IdentityChecks.NewRow()
	err := s.DB.QueryRowContext(ctx, `SELECT national_id_sealed, birth_date_sealed, `+rowseal.IdentityChecks.Fields("")+`
		FROM identity_checks WHERE id = $1 AND status = 'pending'`, id).Scan(append([]any{&sealedID, &sealedBirth}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := rowseal.Check(ctx, rowseal.IdentityChecks, row); err != nil {
		return err
	}
	nid, err := s.Sealer.OpenField(checkAAD(id, "national_id_sealed"), sealedID)
	if err != nil {
		return fmt.Errorf("open the national ID: %w", err)
	}
	birth, err := s.Sealer.OpenField(checkAAD(id, "birth_date_sealed"), sealedBirth)
	if err != nil {
		return fmt.Errorf("open the date of birth: %w", err)
	}
	date, err := time.Parse(time.DateOnly, string(birth))
	if err != nil {
		return errors.New("the date of birth of the check is not a date")
	}
	person := identity.Person{NationalID: string(nid), BirthDate: date}
	match, askErr := s.ask(ctx, userHK, person)
	return s.settle(ctx, userHK, id, person, false, match, askErr, audit.Event{ActorType: audit.ActorSystem})
}
