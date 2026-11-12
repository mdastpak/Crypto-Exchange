package adminauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// ErrSessionEnded is the answer to a token whose session was ended, has
// expired, or does not exist.
var ErrSessionEnded = errors.New("the administrator session has ended")

// Why a session ended, as stored in admin_sessions.revoked_reason.
const (
	SessionLoggedOut       = "logout"
	SessionRevoked         = "revoked"
	SessionReplaced        = "replaced_at_sign_in"
	SessionAdminDisabled   = "administrator_disabled"
	SessionAuthenticator   = "authenticator_reset"
	SessionEndedByOperator = "operator"
	SessionStepUpFailed    = "step_up_failed"
)

// MaxStepUpFailures is how many wrong authenticator codes a session may
// give to step up. The next one ends it, so that a stolen token cannot be
// used to guess codes.
const MaxStepUpFailures = 5

// maxUserAgentLength is what admin_sessions keeps of a User-Agent header.
const maxUserAgentLength = 500

// lastUsedResolution is how often a session in use writes last_used_at, so
// that a request does not write on every check.
const lastUsedResolution = time.Minute

// NewSession is the session of a sign-in. Its ID is in the access token.
type NewSession struct {
	ID        string
	AdminID   string
	IP        string
	UserAgent string
	ExpiresAt time.Time
	// Max is how many sessions the administrator may have at once. The
	// oldest ones past it end with the sign-in.
	Max int
	// ElevatedFor is how long the sign-in, which checked an authenticator
	// code, counts as a step-up.
	ElevatedFor time.Duration
}

// Session is an active session, as its administrator sees it.
type Session struct {
	ID         string     `json:"id"`
	IP         string     `json:"ip,omitempty"`
	UserAgent  string     `json:"user_agent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ElevatedUntil is until when the session may use the sensitive
	// routes without a new authenticator code. It is absent once passed.
	ElevatedUntil *time.Time `json:"elevated_until,omitempty"`
	// Current marks the session of the request that lists.
	Current bool `json:"current"`
}

// NewSessionID returns the ID of a session to be started.
func NewSessionID() string { return uuid.NewString() }

// StartSession stores the session of a sign-in and records event, the
// sign-in, in one transaction: an administrator signs in only with a record
// of it. The administrator's row is locked, so that sign-ins on several
// instances count the sessions one after another, and the oldest sessions
// past s.Max end.
func (s *Service) StartSession(ctx context.Context, session NewSession, event audit.Event) error {
	if session.Max < 1 {
		session.Max = 1
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT hk FROM users WHERE hk = $1 AND role = 'admin' FOR UPDATE`,
			session.AdminID).Scan(&locked); err != nil {
			return err
		}
		written, err := sessionIDs(tx.QueryContext(ctx, `
			UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $3
			WHERE id IN (
				SELECT id FROM admin_sessions
				WHERE admin_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
				ORDER BY created_at DESC, id DESC OFFSET $2)
			RETURNING id::text`,
			session.AdminID, session.Max-1, SessionReplaced))
		if err != nil {
			return err
		}
		var ip interface{}
		if net.ParseIP(session.IP) != nil {
			ip = session.IP
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO admin_sessions (id, admin_id, ip, user_agent, expires_at, elevated_until)
			VALUES ($1, $2, $3, $4, $5, NOW() + make_interval(secs => $6))`,
			session.ID, session.AdminID, ip, shorten(session.UserAgent, maxUserAgentLength), session.ExpiresAt,
			session.ElevatedFor.Seconds()); err != nil {
			return err
		}
		if err := sealSessions(ctx, tx, append(written, session.ID)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		return fmt.Errorf("start the administrator session: %w", err)
	}
	return nil
}

// sessionIDs returns the IDs that a statement with RETURNING id::text
// returned: the sessions it wrote.
func sessionIDs(rows *sql.Rows, err error) ([]string, error) {
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

// sealSessions seals the sessions with ids, which the transaction tx just
// wrote (#337): only the backend can then revoke, extend, or elevate a
// session unnoticed.
func sealSessions(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return rowseal.Reseal(ctx, tx, rowseal.AdminSessions, "id = ANY($1::uuid[])", ids)
}

// readSession reads the sealed columns of an active session of adminID and
// checks them (#337). It returns ErrSessionEnded for a session that is not
// active, and, in enforce mode, for one that fails its check: a revoked
// session set back to active in SQL, or one extended there. extra are read
// first, into dest.
func (s *Service) readSession(ctx context.Context, sessionID, adminID, extra string, dest []any, args ...any) error {
	row := rowseal.AdminSessions.NewRow()
	query := `SELECT ` + rowseal.AdminSessions.Fields("")
	if extra != "" {
		query = `SELECT ` + extra + `, ` + rowseal.AdminSessions.Fields("")
	}
	err := s.db.QueryRowContext(ctx, query+`
		FROM admin_sessions
		WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL AND expires_at > NOW()`,
		append([]any{sessionID, adminID}, args...)...).Scan(append(dest, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionEnded
	}
	if err != nil {
		return fmt.Errorf("check the administrator session: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.AdminSessions, row); errors.Is(err, rowseal.ErrUnverified) {
		return ErrSessionEnded
	} else if err != nil {
		return err
	}
	return nil
}

// shorten returns at most limit characters of s, or nil for none.
func shorten(s string, limit int) interface{} {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit])
	}
	return s
}

// VerifySession checks that the session of an admin token is still active,
// and notes its use. It returns ErrSessionEnded otherwise.
func (s *Service) VerifySession(ctx context.Context, sessionID, adminID string) error {
	if _, err := uuid.Parse(sessionID); err != nil {
		return ErrSessionEnded
	}
	var stale bool
	if err := s.readSession(ctx, sessionID, adminID, `last_used_at IS NULL OR last_used_at < NOW() - make_interval(secs => $3)`,
		[]any{&stale}, lastUsedResolution.Seconds()); err != nil {
		return err
	}
	if stale {
		// The session is valid without the note; a failure to write it
		// must not refuse the request.
		_, _ = s.db.ExecContext(ctx, `UPDATE admin_sessions SET last_used_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	}
	return nil
}

// ListSessions returns the active sessions of an administrator, newest first.
func (s *Service) ListSessions(ctx context.Context, adminID, current string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(host(ip), ''), COALESCE(user_agent, ''), created_at, expires_at, last_used_at,
			CASE WHEN elevated_until > NOW() THEN elevated_until END
		FROM admin_sessions
		WHERE admin_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
		ORDER BY created_at DESC, id DESC`, adminID)
	if err != nil {
		return nil, fmt.Errorf("list the administrator sessions: %w", err)
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		var item Session
		var lastUsed, elevated sql.NullTime
		if err := rows.Scan(&item.ID, &item.IP, &item.UserAgent, &item.CreatedAt, &item.ExpiresAt, &lastUsed, &elevated); err != nil {
			return nil, fmt.Errorf("list the administrator sessions: %w", err)
		}
		if elevated.Valid {
			item.ElevatedUntil = &elevated.Time
		}
		if lastUsed.Valid {
			item.LastUsedAt = &lastUsed.Time
		}
		item.Current = item.ID == current
		sessions = append(sessions, item)
	}
	return sessions, rows.Err()
}

// EndSession ends one active session of an administrator and records event
// in the same transaction. It returns ErrSessionEnded when the administrator
// has no such active session.
func (s *Service) EndSession(ctx context.Context, sessionID, adminID, reason string, event audit.Event) error {
	if _, err := uuid.Parse(sessionID); err != nil {
		return ErrSessionEnded
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ended, err := sessionIDs(tx.QueryContext(ctx, `
			UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $3
			WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL AND expires_at > NOW()
			RETURNING id::text`,
			sessionID, adminID, reason))
		if err != nil {
			return fmt.Errorf("end the administrator session: %w", err)
		}
		if len(ended) == 0 {
			return ErrSessionEnded
		}
		if err := sealSessions(ctx, tx, ended); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, event); err != nil {
			return fmt.Errorf("end the administrator session: %w", err)
		}
		return nil
	})
}

// endSessions ends every active session of an administrator, in the
// transaction of the change that takes their access away.
func endSessions(ctx context.Context, tx *sql.Tx, adminID, reason string) (int64, error) {
	ended, err := sessionIDs(tx.QueryContext(ctx, `
		UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $2
		WHERE admin_id = $1 AND revoked_at IS NULL
		RETURNING id::text`, adminID, reason))
	if err != nil {
		return 0, err
	}
	return int64(len(ended)), sealSessions(ctx, tx, ended)
}

// EndSessions ends every active session of the administrator with email, for
// an operator who suspects a stolen token. It returns how many ended.
func (s *Service) EndSessions(ctx context.Context, email string) (int64, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return 0, err
	}
	var ended int64
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var adminID string
		err := tx.QueryRowContext(ctx, `SELECT hk FROM users WHERE role = 'admin' AND `+ownsEmail+` FOR UPDATE`,
			db.ContactArgs(emailContact, email)...).Scan(&adminID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("find the administrator: %w", err)
		}
		if ended, err = endSessions(ctx, tx, adminID, SessionEndedByOperator); err != nil {
			return fmt.Errorf("end the administrator sessions: %w", err)
		}
		e := audit.Of(s.Origin, audit.ActionAdminSessionsEnd, audit.TargetUser, adminID)
		e.Changes = &audit.Changes{After: map[string]interface{}{"sessions_ended": ended}}
		return audit.Record(ctx, tx, e)
	})
	return ended, err
}

// Elevated reports whether an active session may use the sensitive routes
// now: it signed in, or stepped up, within the step-up time.
func (s *Service) Elevated(ctx context.Context, sessionID, adminID string) (bool, error) {
	if _, err := uuid.Parse(sessionID); err != nil {
		return false, nil
	}
	// A step-up lengthened in SQL fails the check of the session (#337),
	// and counts as none.
	var elevated bool
	err := s.readSession(ctx, sessionID, adminID, `COALESCE(elevated_until > NOW(), FALSE)`, []any{&elevated})
	if errors.Is(err, ErrSessionEnded) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the step-up of the session: %w", err)
	}
	return elevated, nil
}

// StepUp elevates a session for lifetime with a fresh authenticator code,
// which is accepted once, and records event in the same transaction. A wrong
// code returns ErrInvalidCode and is counted on the session; past
// MaxStepUpFailures the session ends, and StepUp returns ErrSessionEnded.
// The failures are recorded with event too, as a failure with its reason.
func (s *Service) StepUp(ctx context.Context, sessionID, adminID, code string, lifetime time.Duration, event audit.Event) (time.Time, error) {
	if err := s.VerifySession(ctx, sessionID, adminID); err != nil {
		return time.Time{}, err
	}
	err := s.VerifyTOTP(ctx, adminID, code)
	if errors.Is(err, ErrInvalidCode) {
		return time.Time{}, s.failStepUp(ctx, sessionID, adminID, event)
	}
	if err != nil {
		return time.Time{}, err
	}
	var until time.Time
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE admin_sessions SET elevated_until = NOW() + make_interval(secs => $3), step_up_failures = 0
			WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL AND expires_at > NOW()
			RETURNING elevated_until`, sessionID, adminID, lifetime.Seconds()).Scan(&until)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionEnded
		}
		if err != nil {
			return fmt.Errorf("step up the session: %w", err)
		}
		if err := sealSessions(ctx, tx, []string{sessionID}); err != nil {
			return err
		}
		event.Outcome, event.Reason = audit.Success, ""
		if err := audit.Record(ctx, tx, event); err != nil {
			return fmt.Errorf("step up the session: %w", err)
		}
		return nil
	})
	return until, err
}

// failStepUp counts a wrong code on the session and ends the session past
// the limit, with the event of the failure.
func (s *Service) failStepUp(ctx context.Context, sessionID, adminID string, event audit.Event) error {
	result := ErrInvalidCode
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var failures int
		err := tx.QueryRowContext(ctx, `
			UPDATE admin_sessions SET step_up_failures = step_up_failures + 1
			WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL
			RETURNING step_up_failures`, sessionID, adminID).Scan(&failures)
		if errors.Is(err, sql.ErrNoRows) {
			result = ErrSessionEnded
			return nil
		}
		if err != nil {
			return err
		}
		event.Outcome, event.Reason = audit.Failure, "authenticator_code_invalid"
		if failures > MaxStepUpFailures {
			if _, err := tx.ExecContext(ctx, `
				UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $2 WHERE id = $1`,
				sessionID, SessionStepUpFailed); err != nil {
				return err
			}
			if err := sealSessions(ctx, tx, []string{sessionID}); err != nil {
				return err
			}
			event.Reason = "too_many_wrong_codes"
			result = ErrSessionEnded
		}
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		return fmt.Errorf("count the wrong step-up code: %w", err)
	}
	return result
}
