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
)

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
}

// Session is an active session, as its administrator sees it.
type Session struct {
	ID         string     `json:"id"`
	IP         string     `json:"ip,omitempty"`
	UserAgent  string     `json:"user_agent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
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
		if _, err := tx.ExecContext(ctx, `
			UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $3
			WHERE id IN (
				SELECT id FROM admin_sessions
				WHERE admin_id = $1 AND revoked_at IS NULL AND expires_at > NOW()
				ORDER BY created_at DESC, id DESC OFFSET $2)`,
			session.AdminID, session.Max-1, SessionReplaced); err != nil {
			return err
		}
		var ip interface{}
		if net.ParseIP(session.IP) != nil {
			ip = session.IP
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO admin_sessions (id, admin_id, ip, user_agent, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			session.ID, session.AdminID, ip, shorten(session.UserAgent, maxUserAgentLength), session.ExpiresAt); err != nil {
			return err
		}
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		return fmt.Errorf("start the administrator session: %w", err)
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
	err := s.db.QueryRowContext(ctx, `
		SELECT last_used_at IS NULL OR last_used_at < NOW() - make_interval(secs => $3)
		FROM admin_sessions
		WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL AND expires_at > NOW()`,
		sessionID, adminID, lastUsedResolution.Seconds()).Scan(&stale)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionEnded
	}
	if err != nil {
		return fmt.Errorf("check the administrator session: %w", err)
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
		SELECT id, COALESCE(host(ip), ''), COALESCE(user_agent, ''), created_at, expires_at, last_used_at
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
		var lastUsed sql.NullTime
		if err := rows.Scan(&item.ID, &item.IP, &item.UserAgent, &item.CreatedAt, &item.ExpiresAt, &lastUsed); err != nil {
			return nil, fmt.Errorf("list the administrator sessions: %w", err)
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
		result, err := tx.ExecContext(ctx, `
			UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $3
			WHERE id = $1 AND admin_id = $2 AND revoked_at IS NULL AND expires_at > NOW()`,
			sessionID, adminID, reason)
		if err != nil {
			return fmt.Errorf("end the administrator session: %w", err)
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			return ErrSessionEnded
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
	result, err := tx.ExecContext(ctx, `
		UPDATE admin_sessions SET revoked_at = NOW(), revoked_reason = $2
		WHERE admin_id = $1 AND revoked_at IS NULL`, adminID, reason)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
			emailContact, email).Scan(&adminID)
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
