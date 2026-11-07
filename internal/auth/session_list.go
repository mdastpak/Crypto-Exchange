package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
)

// SessionInfo is an active session, as its user sees it in the session
// list (#193). It holds no token or hash.
type SessionInfo struct {
	ID         string    `json:"id" format:"uuid"`
	AuthMethod string    `json:"auth_method" example:"otp"`
	UserAgent  string    `json:"user_agent,omitempty"`
	IP         string    `json:"ip,omitempty" example:"203.0.113.7"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	// Current marks the session of the request.
	Current bool `json:"current"`
}

// SessionPage is a page of the session list.
type SessionPage = pagination.Page[SessionInfo]

// SessionPageDoc describes SessionPage in the OpenAPI document, which swag
// cannot read from the generic type.
type SessionPageDoc struct {
	Items []SessionInfo `json:"items"`
	// NextCursor asks for the next page. It is left out on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// SessionManager lets users list and end their own sessions.
// PostgresSessionStore implements it.
type SessionManager interface {
	// List returns the active sessions of userID, newest first.
	List(ctx context.Context, userID string, page pagination.Request) (SessionPage, error)
	// RevokeOwn ends one active session of userID, and records event. It
	// returns ErrSessionInvalid when userID has no such active session,
	// so a user learns nothing about the sessions of others.
	RevokeOwn(ctx context.Context, sessionID, userID string, event audit.Event) error
	// RevokeOthers ends every active session of userID but keep, records
	// event for each, and returns how many it ended.
	RevokeOthers(ctx context.Context, userID, keep string, event audit.Event) (int, error)
}

// sessionPosition is the sort key of the session list in its cursor.
type sessionPosition struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"id"`
}

func (s *PostgresSessionStore) List(ctx context.Context, userID string, page pagination.Request) (SessionPage, error) {
	var after sessionPosition
	hasAfter, err := page.After(&after)
	if err != nil {
		return SessionPage{}, err
	}
	var afterTime interface{}
	if hasAfter {
		afterTime = after.CreatedAt
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, auth_method, COALESCE(user_agent, ''), COALESCE(host(ip), ''), created_at,
			COALESCE(last_used_at, created_at), expires_at
		FROM sessions
		WHERE user_id = $1 AND status = 'ACTIVE' AND expires_at > NOW()
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`,
		userID, afterTime, nullUUID(after.ID), page.Limit+1)
	if err != nil {
		return SessionPage{}, fmt.Errorf("failed to list sessions: %w", err)
	}
	defer rows.Close()
	var sessions []SessionInfo
	for rows.Next() {
		var info SessionInfo
		if err := rows.Scan(&info.ID, &info.AuthMethod, &info.UserAgent, &info.IP, &info.CreatedAt, &info.LastUsedAt, &info.ExpiresAt); err != nil {
			return SessionPage{}, fmt.Errorf("failed to list sessions: %w", err)
		}
		sessions = append(sessions, info)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("failed to list sessions: %w", err)
	}
	return pagination.NewPage(sessions, page, func(info SessionInfo) interface{} {
		return sessionPosition{CreatedAt: info.CreatedAt, ID: info.ID}
	})
}

// nullUUID is NULL for an empty ID, which the first page has.
func nullUUID(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}

func (s *PostgresSessionStore) RevokeOwn(ctx context.Context, sessionID, userID string, event audit.Event) error {
	return s.inTx(ctx, func(tx *sql.Tx) (*audit.Event, bool, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
			WHERE id = $1 AND user_id = $2 AND status = 'ACTIVE' AND expires_at > NOW()`,
			sessionID, userID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to revoke session: %w", err)
		}
		if rows, err := res.RowsAffected(); err != nil {
			return nil, false, fmt.Errorf("failed to revoke session: %w", err)
		} else if rows == 0 {
			return nil, false, ErrSessionInvalid
		}
		return &event, true, nil
	})
}

func (s *PostgresSessionStore) RevokeOthers(ctx context.Context, userID, keep string, event audit.Event) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to start the session transaction: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE user_id = $1 AND id <> $2 AND status = 'ACTIVE' AND expires_at > NOW()
		RETURNING id`,
		userID, keep)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke sessions: %w", err)
	}
	var revoked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to revoke sessions: %w", err)
		}
		revoked = append(revoked, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("failed to revoke sessions: %w", err)
	}
	// One event per session, each under its own savepoint, so that the
	// revocations commit even when the audit log cannot take them.
	for _, id := range revoked {
		e := event
		e.TargetType, e.TargetID = audit.TargetSession, id
		if err := recordInTx(ctx, tx, e, true); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit the session change: %w", err)
	}
	return len(revoked), nil
}
