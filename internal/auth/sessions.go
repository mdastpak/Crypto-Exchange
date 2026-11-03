package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrSessionInvalid means the session does not exist, has expired, or was
	// revoked, so its refresh token can no longer be used.
	ErrSessionInvalid = errors.New("session is not active")
	// ErrRefreshTokenReused means a refresh token that had already been rotated
	// was presented again. It may have been stolen, so the session is revoked.
	ErrRefreshTokenReused = fmt.Errorf("%w: refresh token was already used", ErrSessionInvalid)
)

// SessionStore persists sign-in sessions so that refresh tokens can be rotated
// and revoked. Only hashes of tokens are stored.
type SessionStore interface {
	Create(ctx context.Context, session Session) error
	Rotate(ctx context.Context, rotation SessionRotation) error
	Revoke(ctx context.Context, sessionID, userID string) error
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
