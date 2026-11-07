package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"

	"github.com/gin-gonic/gin"
)

var (
	// ErrSessionInvalid means the session does not exist, has expired, or was
	// revoked, so its refresh token can no longer be used.
	ErrSessionInvalid = errors.New("session is not active")
	// ErrRefreshTokenReused means a refresh token that had already been rotated
	// was presented again. It may have been stolen, so the session is revoked.
	ErrRefreshTokenReused = fmt.Errorf("%w: refresh token was already used", ErrSessionInvalid)
	// ErrAccountUnavailable means the session's user can no longer sign in:
	// the account was disabled, suspended, banned, deleted, or made an
	// administrator after the session began.
	ErrAccountUnavailable = errors.New("account is not available")
)

// SessionStore persists sign-in sessions so that refresh tokens can be rotated
// and revoked. Only hashes of tokens are stored.
//
// Create, Rotate, and Revoke write the audit event of their change in the
// transaction of the change (#51). A change that grants access, a new
// session or new tokens, never commits without its event. A revocation
// commits even when its event cannot be written, so that an unavailable audit
// log never keeps a session alive.
type SessionStore interface {
	Create(ctx context.Context, session Session, event audit.Event) error
	// Rotate records event when it rotates the tokens. When it revokes the
	// session instead, because the refresh token was already used, it
	// records event as a failure with ReasonRefreshTokenReused.
	Rotate(ctx context.Context, rotation SessionRotation, event audit.Event) error
	Revoke(ctx context.Context, sessionID, userID string, event audit.Event) error
	// Note records an event that changes no session, such as a refused
	// refresh.
	Note(ctx context.Context, event audit.Event) error
	// Verify reports whether the tokens of a session may still be used. It
	// returns ErrSessionInvalid when the session does not exist, belongs to
	// another user, has expired, or was revoked, and ErrAccountUnavailable
	// when its user can no longer sign in.
	Verify(ctx context.Context, sessionID, userID string) error
}

// Session is a new sign-in session.
type Session struct {
	ID               string
	UserID           string
	AuthMethod       string
	AccessTokenHash  string
	RefreshTokenHash string
	UserAgent        string
	IP               string
	ExpiresAt        time.Time
}

// SessionRotation replaces the tokens of a session, provided that the
// presented refresh token is the session's current one.
type SessionRotation struct {
	SessionID            string
	UserID               string
	PresentedRefreshHash string
	NewRefreshHash       string
	NewAccessHash        string
	ExpiresAt            time.Time
}

// ReasonRefreshTokenReused is the audit reason of a refresh token that was
// presented again after its rotation.
const ReasonRefreshTokenReused = "refresh_token_reused"

// reasonSessionInactive is the audit reason, and the error code, of a token
// whose session has ended.
const reasonSessionInactive = "session_inactive"

// SessionEvent returns the successful event of action on a user's session,
// with the request's ID, client address, and user agent.
func SessionEvent(c *gin.Context, action, userID, sessionID string) audit.Event {
	e := audit.FromRequest(c)
	e.ActorType, e.ActorID = audit.ActorUser, userID
	e.Action, e.Outcome = action, audit.Success
	e.TargetType, e.TargetID = audit.TargetSession, sessionID
	return e
}

// HashToken returns the hex-encoded SHA-256 hash under which a token is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionTouchInterval is how often the use of a session is recorded.
const sessionTouchInterval = time.Minute

// PostgresSessionStore keeps sessions in the sessions table.
type PostgresSessionStore struct {
	DB *sql.DB
}

func NewPostgresSessionStore(db *sql.DB) *PostgresSessionStore {
	return &PostgresSessionStore{DB: db}
}

// sessionChange is a change of sessions in a transaction. It returns the
// event to record with it, if any; whether it revokes, rather than grants,
// access; and the error to report, which may come with an event, as a
// detected reuse does.
type sessionChange func(tx *sql.Tx) (event *audit.Event, revokes bool, err error)

// auditSavepoint lets a revocation commit without the event that could not
// be written.
const auditSavepoint = "session_audit_event"

// inTx runs change and records its event in one transaction. A change that
// grants access commits only with its event; a revocation commits without it
// when it cannot be written, and the failure is logged.
func (s *PostgresSessionStore) inTx(ctx context.Context, change sessionChange) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start the session transaction: %w", err)
	}
	defer tx.Rollback()
	event, revokes, changeErr := change(tx)
	if event == nil {
		return changeErr
	}
	if err := recordInTx(ctx, tx, *event, revokes); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit the session change: %w", err)
	}
	return changeErr
}

// recordInTx records event in tx. For a revocation, it records the event
// under a savepoint, and on failure rolls back only the event.
func recordInTx(ctx context.Context, tx *sql.Tx, event audit.Event, revokes bool) error {
	if !revokes {
		return audit.Record(ctx, tx, event)
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+auditSavepoint); err != nil {
		return fmt.Errorf("failed to protect the revocation from its audit event: %w", err)
	}
	recordErr := audit.Record(ctx, tx, event)
	if recordErr == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+auditSavepoint); err != nil {
		return fmt.Errorf("failed to keep the revocation without its audit event: %w (recording: %w)", err, recordErr)
	}
	log.Printf("Revoked session %s without its audit event %s: %v", event.TargetID, event.Action, recordErr)
	return nil
}

func (s *PostgresSessionStore) Create(ctx context.Context, session Session, event audit.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) (*audit.Event, bool, error) {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, user_id, auth_method, access_token_hash, refresh_token_hash, user_agent, ip, expires_at, last_used_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::inet, $8, NOW())`,
			session.ID, session.UserID, session.AuthMethod, session.AccessTokenHash, session.RefreshTokenHash,
			session.UserAgent, session.IP, session.ExpiresAt,
		)
		if err != nil {
			return nil, false, fmt.Errorf("failed to create session: %w", err)
		}
		return &event, false, nil
	})
}

// Rotate swaps the session's tokens in a single conditional update, so that two
// requests presenting the same refresh token cannot both succeed.
func (s *PostgresSessionStore) Rotate(ctx context.Context, r SessionRotation, event audit.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) (*audit.Event, bool, error) {
		return rotate(ctx, tx, r, event)
	})
}

// rotate rotates the tokens, or revokes a session whose refresh token was
// already used. It is a sessionChange.
func rotate(ctx context.Context, tx *sql.Tx, r SessionRotation, event audit.Event) (*audit.Event, bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET refresh_token_hash = $1, access_token_hash = $2, expires_at = $3, last_used_at = NOW()
		WHERE id = $4 AND user_id = $5 AND refresh_token_hash = $6
		  AND status = 'ACTIVE' AND expires_at > NOW()`,
		r.NewRefreshHash, r.NewAccessHash, r.ExpiresAt, r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return nil, false, fmt.Errorf("failed to rotate session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("failed to rotate session: %w", err)
	} else if rows == 1 {
		return &event, false, nil
	}

	// Nothing was rotated. If the session is still active, the presented token
	// is a valid but already rotated refresh token: revoke the session.
	res, err = tx.ExecContext(ctx, `
		UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND refresh_token_hash <> $3
		  AND status = 'ACTIVE' AND expires_at > NOW()`,
		r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return nil, false, fmt.Errorf("failed to revoke reused session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("failed to revoke reused session: %w", err)
	} else if rows == 1 {
		event.Outcome, event.Reason = audit.Failure, ReasonRefreshTokenReused
		return &event, true, ErrRefreshTokenReused
	}
	return nil, false, ErrSessionInvalid
}

// Verify reads the session and its user in one lookup by primary key. It
// reads the database on every call, without a cache, so that a change made
// directly in the database, such as suspending a user, takes effect on the
// next request. A session that is no longer active is reported before an
// account that is no longer available, because its tokens are dead either
// way.
func (s *PostgresSessionStore) Verify(ctx context.Context, sessionID, userID string) error {
	var active, deleted bool
	var status, role string
	// The same statement records the use of an active session, at most
	// once a minute, so that most requests write nothing (#193).
	err := s.DB.QueryRowContext(ctx, `
		WITH found AS (
			SELECT s.id, s.status = 'ACTIVE' AND s.expires_at > NOW() AS active, s.last_used_at,
				u.status AS user_status, u.role, u.deleted_at IS NOT NULL AS deleted
			FROM sessions s JOIN users u ON u.hk = s.user_id
			WHERE s.id = $1 AND s.user_id = $2
		), touched AS (
			UPDATE sessions SET last_used_at = NOW() FROM found
			WHERE sessions.id = found.id AND found.active
			  AND (found.last_used_at IS NULL OR found.last_used_at < NOW() - make_interval(secs => $3))
		)
		SELECT active, user_status, role, deleted FROM found`,
		sessionID, userID, sessionTouchInterval.Seconds(),
	).Scan(&active, &status, &role, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionInvalid
	}
	if err != nil {
		return fmt.Errorf("failed to verify session: %w", err)
	}
	if !active {
		return ErrSessionInvalid
	}
	if deleted || role != RoleUser || !account.UserStatus(status).CanSignIn() {
		return ErrAccountUnavailable
	}
	return nil
}

func (s *PostgresSessionStore) Revoke(ctx context.Context, sessionID, userID string, event audit.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) (*audit.Event, bool, error) {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
			WHERE id = $1 AND user_id = $2 AND status = 'ACTIVE'`,
			sessionID, userID,
		)
		if err != nil {
			return nil, false, fmt.Errorf("failed to revoke session: %w", err)
		}
		return &event, true, nil
	})
}

func (s *PostgresSessionStore) Note(ctx context.Context, event audit.Event) error {
	return audit.Record(ctx, s.DB, event)
}
