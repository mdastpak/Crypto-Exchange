package restrictions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// ReasonAccountFrozen is the audit reason of a session that a freeze ended.
const ReasonAccountFrozen = "account_frozen"

// Errors of Create and Lift.
var (
	// ErrNotUser is a restriction of an account that is not a user's: an
	// administrator, or a deleted user.
	ErrNotUser = errors.New("not a user account")
	// ErrCurrencyNotFound is a hold of a currency that does not exist.
	ErrCurrencyNotFound = errors.New("currency not found")
	// ErrNotFound is a restriction that is not the user's.
	ErrNotFound = errors.New("restriction not found")
	// ErrAlreadyLifted is a lift of a lifted restriction.
	ErrAlreadyLifted = errors.New("restriction already lifted")
)

// VersionConflict is a lift based on another version of the restriction.
type VersionConflict struct{ Current int64 }

func (v *VersionConflict) Error() string {
	return fmt.Sprintf("the restriction is at version %d", v.Current)
}

// New is a restriction to create.
type New struct {
	UserHK     string
	Scope      Scope
	Operations []Operation
	CurrencyHK string
	Reason     Reason
	CaseRef    string
	Note       string
	EndsAt     *time.Time
	// CreatedBy is the administrator.
	CreatedBy string
}

// Created is the result of Create.
type Created struct {
	Restriction Restriction
	// EndedSessions are the sessions that a freeze ended.
	EndedSessions int
}

// lockUser takes the exclusive lock of a user's row, which waits for the
// operations of the user in flight, and checks that it is a user's account.
func lockUser(ctx context.Context, tx *sql.Tx, userHK string) error {
	var role string
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT role, deleted_at IS NOT NULL FROM users WHERE hk = $1 FOR UPDATE`, userHK).
		Scan(&role, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to lock the user: %w", err)
	}
	if role != "user" || deleted {
		return ErrNotUser
	}
	return nil
}

func joinOperations(ops []Operation) string {
	parts := make([]string, len(ops))
	for i, op := range ops {
		parts[i] = string(op)
	}
	return strings.Join(parts, ",")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Create records a restriction, and its audit event from origin, in one
// transaction. A freeze ends every active session of the user in the same
// transaction, so that no token of the user works once it commits.
func Create(ctx context.Context, db *sql.DB, n New, origin audit.Event) (Created, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, fmt.Errorf("failed to start the restriction: %w", err)
	}
	defer tx.Rollback()

	if err := lockUser(ctx, tx, n.UserHK); err != nil {
		return Created{}, err
	}
	if n.Scope == ScopeAsset {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM currencies WHERE hk = $1 AND deleted_at IS NULL`, n.CurrencyHK).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return Created{}, ErrCurrencyNotFound
		}
		if err != nil {
			return Created{}, fmt.Errorf("failed to read the currency: %w", err)
		}
	}

	hk := NewHK()
	row := tx.QueryRowContext(ctx, `
		INSERT INTO user_restrictions AS r (hk, user_hk, scope, operations, currency_hk, reason, case_ref, note, ends_at, created_by)
		VALUES ($1, $2, $3, COALESCE(string_to_array(NULLIF($4, ''), ','), '{}')::TEXT[], $5, $6, $7, $8, $9, $10)
		RETURNING `+Columns,
		hk, n.UserHK, n.Scope, joinOperations(n.Operations), nullable(n.CurrencyHK), n.Reason,
		nullable(n.CaseRef), nullable(n.Note), n.EndsAt, n.CreatedBy)
	r, err := Scan(row)
	if err != nil {
		return Created{}, fmt.Errorf("failed to record the restriction: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserRestrictions, "hk = $1", hk); err != nil {
		return Created{}, err
	}
	if r.Operations == nil {
		r.Operations = []Operation{}
	}

	after := map[string]interface{}{"restriction": r.HK, "scope": r.Scope, "operations": r.Operations}
	if r.CurrencyHK != nil {
		after["currency_hk"] = *r.CurrencyHK
	}
	if r.CaseRef != "" {
		after["case_ref"] = r.CaseRef
	}
	if r.EndsAt != nil {
		after["ends_at"] = r.EndsAt.UTC().Format(time.RFC3339)
	}
	event := audit.Of(origin, audit.ActionUserRestrictionAdd, audit.TargetUser, n.UserHK)
	event.Reason = string(n.Reason)
	event.Changes = &audit.Changes{After: after}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Created{}, err
	}

	created := Created{Restriction: r}
	if n.Scope == ScopeAccount {
		rows, err := tx.QueryContext(ctx, `
			UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
			WHERE user_id = $1 AND status = 'ACTIVE' RETURNING id`, n.UserHK)
		if err != nil {
			return Created{}, fmt.Errorf("failed to end the sessions: %w", err)
		}
		var ended []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return Created{}, fmt.Errorf("failed to end the sessions: %w", err)
			}
			ended = append(ended, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Created{}, fmt.Errorf("failed to end the sessions: %w", err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = ANY($1::uuid[])", ended); err != nil {
			return Created{}, err
		}
		for _, id := range ended {
			e := audit.Of(origin, audit.ActionUserSessionRevoke, audit.TargetSession, id)
			e.Reason = ReasonAccountFrozen
			if err := audit.Record(ctx, tx, e); err != nil {
				return Created{}, err
			}
		}
		created.EndedSessions = len(ended)
	}
	if err := tx.Commit(); err != nil {
		return Created{}, fmt.Errorf("failed to record the restriction: %w", err)
	}
	return created, nil
}

// Lift ends a restriction of a user, with an audit event from origin, in
// one transaction. expected, when set, is the version the lift is based on.
func Lift(ctx context.Context, db *sql.DB, userHK, hk, liftedBy, note string, expected *int64, origin audit.Event) (Restriction, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Restriction{}, fmt.Errorf("failed to start the lift: %w", err)
	}
	defer tx.Rollback()

	// The user's row first, as Create does, so that the two never wait for
	// each other in opposite orders.
	if err := lockUser(ctx, tx, userHK); err != nil && !errors.Is(err, ErrNotUser) {
		return Restriction{}, err
	}
	sealed := rowseal.UserRestrictions.NewRow()
	r, err := Scan(withSeal{tx.QueryRowContext(ctx, `SELECT `+Columns+`, `+rowseal.UserRestrictions.Fields("r")+`
		FROM user_restrictions r WHERE r.hk = $1 AND r.user_hk = $2 FOR UPDATE`, hk, userHK), sealed.Dest()})
	if errors.Is(err, sql.ErrNoRows) {
		return Restriction{}, ErrNotFound
	}
	if err != nil {
		return Restriction{}, fmt.Errorf("failed to read the restriction: %w", err)
	}
	// A restriction changed in SQL is lifted only after its finding is
	// resolved (#337): its values are not the ones an administrator set.
	if err := rowseal.Check(ctx, rowseal.UserRestrictions, sealed); err != nil {
		return Restriction{}, err
	}
	if expected != nil && *expected != r.Version {
		return Restriction{}, &VersionConflict{Current: r.Version}
	}
	if r.LiftedAt != nil {
		return Restriction{}, ErrAlreadyLifted
	}

	r, err = Scan(tx.QueryRowContext(ctx, `
		UPDATE user_restrictions AS r SET lifted_at = NOW(), lifted_by = $2, lift_note = $3, version = version + 1
		WHERE r.hk = $1 RETURNING `+Columns, hk, liftedBy, note))
	if err != nil {
		return Restriction{}, fmt.Errorf("failed to lift the restriction: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserRestrictions, "hk = $1", hk); err != nil {
		return Restriction{}, err
	}
	event := audit.Of(origin, audit.ActionUserRestrictionLift, audit.TargetUser, userHK)
	event.Reason = string(r.Reason)
	event.Changes = &audit.Changes{
		Before: map[string]interface{}{"restriction": r.HK, "scope": r.Scope, "lifted": false},
		After:  map[string]interface{}{"restriction": r.HK, "scope": r.Scope, "lifted": true},
	}
	if err := audit.Record(ctx, tx, event); err != nil {
		return Restriction{}, err
	}
	if err := tx.Commit(); err != nil {
		return Restriction{}, fmt.Errorf("failed to lift the restriction: %w", err)
	}
	return r, nil
}

// withSeal scans a row of Columns followed by the sealed columns of
// rowseal.UserRestrictions into seal.
type withSeal struct {
	row  scanner
	seal []any
}

func (w withSeal) Scan(dest ...any) error { return w.row.Scan(append(dest, w.seal...)...) }

// Active returns the restrictions of a user that apply now, newest first.
// It reads every restriction of the user, also the lifted and the ended
// ones, and checks each against its seal (#337): a freeze lifted, or ended
// early, in SQL no longer looks active, so a read of the active ones alone
// would not see it. In enforce mode, a restriction that fails its check
// returns rowseal.ErrUnverified, which refuses what the restrictions guard.
func Active(ctx context.Context, q Querier, userHK string) ([]Restriction, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+Columns+`, `+rowseal.UserRestrictions.Fields("r")+`
		FROM user_restrictions r WHERE r.user_hk = $1 ORDER BY r.created_at DESC, r.hk DESC LIMIT $2`, userHK, maxActive)
	if err != nil {
		return nil, fmt.Errorf("failed to read the restrictions: %w", err)
	}
	var all []Restriction
	var seals []*rowseal.Row
	for rows.Next() {
		sealed := rowseal.UserRestrictions.NewRow()
		r, err := Scan(withSeal{rows, sealed.Dest()})
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to read the restrictions: %w", err)
		}
		all, seals = append(all, r), append(seals, sealed)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read the restrictions: %w", err)
	}
	active := []Restriction{}
	for i, r := range all {
		if err := rowseal.Check(ctx, rowseal.UserRestrictions, seals[i]); err != nil {
			return nil, err
		}
		if r.Active {
			active = append(active, r)
		}
	}
	return active, nil
}

// List returns the restrictions of a user, newest first: every one, or only
// those that apply now. It returns at most limit. It is the view of an
// administrator, and checks no seal; the verifier checks every row.
func List(ctx context.Context, q Querier, userHK string, activeOnly bool, limit int) ([]Restriction, error) {
	query := `SELECT ` + Columns + ` FROM user_restrictions r WHERE r.user_hk = $1`
	if activeOnly {
		query += ` AND ` + ActiveSQL
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY r.created_at DESC, r.hk DESC LIMIT $2`, userHK, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to read the restrictions: %w", err)
	}
	defer rows.Close()
	list := []Restriction{}
	for rows.Next() {
		r, err := Scan(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to read the restrictions: %w", err)
		}
		list = append(list, r)
	}
	return list, rows.Err()
}
