package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/account"
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
type SessionStore interface {
	Create(ctx context.Context, session Session) error
	Rotate(ctx context.Context, rotation SessionRotation) error
	Revoke(ctx context.Context, sessionID, userID string) error
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

// HashToken returns the hex-encoded SHA-256 hash under which a token is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// PostgresSessionStore keeps sessions in the sessions table.
type PostgresSessionStore struct {
	DB *sql.DB
}

func NewPostgresSessionStore(db *sql.DB) *PostgresSessionStore {
	return &PostgresSessionStore{DB: db}
}

func (s *PostgresSessionStore) Create(ctx context.Context, session Session) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, auth_method, access_token_hash, refresh_token_hash, user_agent, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::inet, $8)`,
		session.ID, session.UserID, session.AuthMethod, session.AccessTokenHash, session.RefreshTokenHash,
		session.UserAgent, session.IP, session.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

// Rotate swaps the session's tokens in a single conditional update, so that two
// requests presenting the same refresh token cannot both succeed.
func (s *PostgresSessionStore) Rotate(ctx context.Context, r SessionRotation) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE sessions
		SET refresh_token_hash = $1, access_token_hash = $2, expires_at = $3
		WHERE id = $4 AND user_id = $5 AND refresh_token_hash = $6
		  AND status = 'ACTIVE' AND expires_at > NOW()`,
		r.NewRefreshHash, r.NewAccessHash, r.ExpiresAt, r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return fmt.Errorf("failed to rotate session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to rotate session: %w", err)
	} else if rows == 1 {
		return nil
	}

	// Nothing was rotated. If the session is still active, the presented token
	// is a valid but already rotated refresh token: revoke the session.
	res, err = s.DB.ExecContext(ctx, `
		UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND refresh_token_hash <> $3
		  AND status = 'ACTIVE' AND expires_at > NOW()`,
		r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke reused session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to revoke reused session: %w", err)
	} else if rows == 1 {
		return ErrRefreshTokenReused
	}
	return ErrSessionInvalid
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
	err := s.DB.QueryRowContext(ctx, `
		SELECT s.status = 'ACTIVE' AND s.expires_at > NOW(), u.status, u.role, u.deleted_at IS NOT NULL
		FROM sessions s JOIN users u ON u.hk = s.user_id
		WHERE s.id = $1 AND s.user_id = $2`,
		sessionID, userID,
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

func (s *PostgresSessionStore) Revoke(ctx context.Context, sessionID, userID string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND status = 'ACTIVE'`,
		sessionID, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke session: %w", err)
	}
	return nil
}
