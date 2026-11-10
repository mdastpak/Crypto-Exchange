package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
)

var (
	// ErrSessionInvalid means the session does not exist, has expired, or was
	// revoked, so its refresh token can no longer be used.
	ErrSessionInvalid = errors.New("session is not active")
	// ErrRefreshTokenReused means a refresh token that had already been rotated
	// was presented again. It may have been stolen, so the session is revoked.
	ErrRefreshTokenReused = fmt.Errorf("%w: refresh token was already used", ErrSessionInvalid)
	// ErrConcurrentRefresh means the refresh token that the session had
	// before its last rotation was presented within the grace period, as
	// when two tabs refresh at once (#194). The session keeps its current
	// refresh token, and the caller issues only a new access token.
	ErrConcurrentRefresh = errors.New("refresh token was rotated by a concurrent refresh")
	// ErrAccountUnavailable means the session's user can no longer sign in:
	// the account was disabled, suspended, banned, deleted, or made an
	// administrator after the session began.
	ErrAccountUnavailable = errors.New("account is not available")
	// ErrAccountFrozen means the account is frozen by a restriction (#220).
	// It is an ErrAccountUnavailable, for callers that do not tell them
	// apart.
	ErrAccountFrozen = fmt.Errorf("%w: frozen", ErrAccountUnavailable)
	// ErrAccountUnverified means the user's row does not match its seal,
	// in enforce mode (#139): it was changed outside the backend. It is an
	// ErrAccountUnavailable.
	ErrAccountUnverified = fmt.Errorf("%w: the account does not match its seal", ErrAccountUnavailable)
	// ErrSessionUnverified means the session's row does not match its seal,
	// in enforce mode (#139). It is an ErrSessionInvalid.
	ErrSessionUnverified = fmt.Errorf("%w: the session does not match its seal", ErrSessionInvalid)
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

// LocaleVerifier is a SessionStore that also returns, with Verify, the
// locale that the user saved in their profile (#286), so that the
// negotiation of a signed-in request needs no other read.
type LocaleVerifier interface {
	VerifyLocale(ctx context.Context, sessionID, userID string) (string, error)
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
	// MaxActive, when above 0, caps the active sessions of the user (#212):
	// Create fails with ErrSessionLimit instead of exceeding it.
	MaxActive int
	// Replace is an active session of the user that Create ends in the
	// same transaction, so that a user at the cap can sign in by giving up
	// one session. Create fails with ErrReplacedSessionNotFound when it is
	// not one.
	Replace string
}

var (
	// ErrSessionLimit reports a user who has Session.MaxActive active
	// sessions already.
	ErrSessionLimit = errors.New("the user has the most active sessions allowed")
	// ErrReplacedSessionNotFound reports a Session.Replace that is not an
	// active session of the user.
	ErrReplacedSessionNotFound = errors.New("the session to replace is not an active session of the user")
)

// ReasonReplacedAtSignIn is the audit reason of a session that a user ended
// to sign in past the session limit.
const ReasonReplacedAtSignIn = "replaced_at_sign_in"

// SessionRotation replaces the tokens of a session, provided that the
// presented refresh token is the session's current one.
type SessionRotation struct {
	SessionID            string
	UserID               string
	PresentedRefreshHash string
	NewRefreshHash       string
	NewAccessHash        string
	ExpiresAt            time.Time
	// Grace is how long after a rotation the previous refresh token is
	// still accepted, for a concurrent refresh. Zero accepts it never.
	Grace time.Duration
}

// ReasonRefreshTokenReused is the audit reason of a refresh token that was
// presented again after its rotation.
const ReasonRefreshTokenReused = "refresh_token_reused"

// ReasonConcurrentRefresh is the audit reason of a refresh that presented
// the previous refresh token within the grace period.
const ReasonConcurrentRefresh = "concurrent_refresh"

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

// SessionMaxLifetime is the absolute lifetime of a session (#144),
// jwt.sessions.max_lifetime_minutes; 0, when it is not configured, as in
// tests that set no configuration, sets no limit.
func SessionMaxLifetime() time.Duration {
	return time.Duration(config.Config.JWT.Sessions.MaxLifetimeMinutes) * time.Minute
}

// liveSession is the SQL condition of a session whose tokens may still be
// used: active, not expired, and younger than SessionMaxLifetime (#144).
// alias qualifies the columns, as "s" does s.status.
//
// The age is checked here, not only kept out of expires_at by rotate, so
// that the limit holds for every session at once, also when the limit is
// lowered.
func liveSession(alias string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	condition := p + "status = 'ACTIVE' AND " + p + "expires_at > NOW()"
	if minutes := config.Config.JWT.Sessions.MaxLifetimeMinutes; minutes > 0 {
		// A number from the configuration, never a value of a request.
		condition += " AND " + p + "created_at > NOW() - make_interval(mins => " + strconv.Itoa(minutes) + ")"
	}
	return condition
}

// cappedExpiry is the SQL expression of a new expires_at, the parameter
// param, which rotate never sets past the creation of the session plus
// SessionMaxLifetime.
func cappedExpiry(param string) string {
	minutes := config.Config.JWT.Sessions.MaxLifetimeMinutes
	if minutes <= 0 {
		return param
	}
	return "LEAST(" + param + ", created_at + make_interval(mins => " + strconv.Itoa(minutes) + "))"
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
		if session.MaxActive > 0 || session.Replace != "" {
			if err := limitSessions(ctx, tx, session, event); err != nil {
				return nil, false, err
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, user_id, auth_method, access_token_hash, refresh_token_hash, user_agent, ip, expires_at, last_used_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::inet, $8, NOW())`,
			session.ID, session.UserID, session.AuthMethod, session.AccessTokenHash, session.RefreshTokenHash,
			session.UserAgent, session.IP, session.ExpiresAt,
		)
		if err != nil {
			return nil, false, fmt.Errorf("failed to create session: %w", err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1", session.ID); err != nil {
			return nil, false, err
		}
		return &event, false, nil
	})
}

// limitSessions ends session.Replace, if set, and then checks that the user
// has fewer than session.MaxActive active sessions, in the transaction that
// creates the new one. It first locks the user's row: sign-ins of one user
// on every instance then count one after the other, so that concurrent
// sign-ins cannot all pass the count and exceed the cap together.
//
// The lock also orders the sign-in with a freeze of the account (#220),
// which takes it too: a sign-in that waited for a freeze sees it and fails
// with ErrAccountFrozen, instead of creating a session that the freeze did
// not end.
func limitSessions(ctx context.Context, tx *sql.Tx, session Session, event audit.Event) error {
	var frozen bool
	err := tx.QueryRowContext(ctx, `SELECT `+restrictions.FrozenSQL("users.hk")+` FROM users WHERE hk = $1 FOR UPDATE`,
		session.UserID).Scan(&frozen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed to lock the user: %w", err)
	}
	if frozen {
		return ErrAccountFrozen
	}
	if session.Replace != "" {
		//nolint:gosec // G202: liveSession and cappedExpiry join constant fragments and a number of the configuration; every value is an argument.
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
			WHERE id = $1 AND user_id = $2 AND `+liveSession(""),
			session.Replace, session.UserID)
		if err != nil {
			return fmt.Errorf("failed to end the replaced session: %w", err)
		}
		if rows, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("failed to end the replaced session: %w", err)
		} else if rows != 1 {
			return ErrReplacedSessionNotFound
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1", session.Replace); err != nil {
			return err
		}
		revoked := event
		revoked.Action, revoked.Outcome, revoked.Reason = audit.ActionUserSessionRevoke, audit.Success, ReasonReplacedAtSignIn
		revoked.TargetType, revoked.TargetID = audit.TargetSession, session.Replace
		if err := audit.Record(ctx, tx, revoked); err != nil {
			return err
		}
	}
	if session.MaxActive <= 0 {
		return nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM sessions WHERE user_id = $1 AND `+liveSession(""),
		session.UserID).Scan(&active); err != nil {
		return fmt.Errorf("failed to count the active sessions: %w", err)
	}
	if active >= session.MaxActive {
		return ErrSessionLimit
	}
	return nil
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
	//nolint:gosec // G202: liveSession and cappedExpiry join constant fragments and a number of the configuration; every value is an argument.
	res, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET refresh_token_hash = $1, access_token_hash = $2, expires_at = `+cappedExpiry("$3::timestamptz")+`, last_used_at = NOW(),
			previous_refresh_hash = refresh_token_hash, rotated_at = NOW()
		WHERE id = $4 AND user_id = $5 AND refresh_token_hash = $6
		  AND `+liveSession(""),
		r.NewRefreshHash, r.NewAccessHash, r.ExpiresAt, r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return nil, false, fmt.Errorf("failed to rotate session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("failed to rotate session: %w", err)
	} else if rows == 1 {
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1", r.SessionID); err != nil {
			return nil, false, err
		}
		return &event, false, nil
	}

	// Nothing was rotated. The token that the last rotation replaced, within
	// the grace period, is a concurrent refresh: it gets a new access token,
	// and the session keeps its current refresh token.
	if r.Grace > 0 {
		//nolint:gosec // G202: liveSession and cappedExpiry join constant fragments and a number of the configuration; every value is an argument.
		res, err = tx.ExecContext(ctx, `
			UPDATE sessions SET access_token_hash = $4, last_used_at = NOW()
			WHERE id = $1 AND user_id = $2 AND previous_refresh_hash = $3
			  AND rotated_at > NOW() - make_interval(secs => $5)
			  AND `+liveSession(""),
			r.SessionID, r.UserID, r.PresentedRefreshHash, r.NewAccessHash, r.Grace.Seconds(),
		)
		if err != nil {
			return nil, false, fmt.Errorf("failed to check a concurrent refresh: %w", err)
		}
		if rows, err := res.RowsAffected(); err != nil {
			return nil, false, fmt.Errorf("failed to check a concurrent refresh: %w", err)
		} else if rows == 1 {
			if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1", r.SessionID); err != nil {
				return nil, false, err
			}
			event.Reason = ReasonConcurrentRefresh
			return &event, false, ErrConcurrentRefresh
		}
	}

	// Otherwise, if the session is still active, the presented token is a
	// valid but already rotated refresh token: revoke the session.
	//nolint:gosec // G202: liveSession joins constant fragments and a number of the configuration; every value is an argument.
	res, err = tx.ExecContext(ctx, `
		UPDATE sessions SET status = 'REVOKED', revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND refresh_token_hash <> $3
		  AND `+liveSession(""),
		r.SessionID, r.UserID, r.PresentedRefreshHash,
	)
	if err != nil {
		return nil, false, fmt.Errorf("failed to revoke reused session: %w", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("failed to revoke reused session: %w", err)
	} else if rows == 1 {
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1", r.SessionID); err != nil {
			return nil, false, err
		}
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
	_, err := s.VerifyLocale(ctx, sessionID, userID)
	return err
}

// VerifyLocale is Verify that also returns the locale the user saved in
// their profile (#286), or "" for none, from the same lookup.
func (s *PostgresSessionStore) VerifyLocale(ctx context.Context, sessionID, userID string) (string, error) {
	var active, deleted, frozen bool
	var status, role, locale string
	sessionRow, userRow := rowseal.Sessions.NewRow(), rowseal.Users.NewRow()
	dest := append([]any{&active, &status, &role, &deleted, &frozen, &locale}, sessionRow.Dest()...)
	dest = append(dest, userRow.Dest()...)
	// The same statement records the use of an active session, at most
	// once a minute, so that most requests write nothing (#193). It also
	// reads the sealed columns of both rows (#139); last_used_at is not one
	// of them, so recording the use needs no new seal.
	err := s.DB.QueryRowContext(ctx, `
		WITH found AS (
			SELECT s.id, (`+liveSession("s")+`) AS active, s.last_used_at,
				u.status AS user_status, u.role, u.deleted_at IS NOT NULL AS deleted,
				COALESCE(u.locale, '') AS locale,
				`+restrictions.FrozenSQL("u.hk")+` AS frozen,
				`+rowseal.Sessions.FieldsAs("s", "sr")+`,
				`+rowseal.Users.FieldsAs("u", "ur")+`
			FROM sessions s JOIN users u ON u.hk = s.user_id
			WHERE s.id = $1 AND s.user_id = $2
		), touched AS (
			UPDATE sessions SET last_used_at = NOW() FROM found
			WHERE sessions.id = found.id AND found.active
			  AND (found.last_used_at IS NULL OR found.last_used_at < NOW() - make_interval(secs => $3))
		)
		SELECT active, user_status, role, deleted, frozen, locale,
			`+rowseal.Sessions.Names("sr")+`, `+rowseal.Users.Names("ur")+` FROM found`,
		sessionID, userID, sessionTouchInterval.Seconds(),
	).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionInvalid
	}
	if err != nil {
		return "", fmt.Errorf("failed to verify session: %w", err)
	}
	// A row changed outside the backend, such as a revoked session or a
	// banned user set back to active, is not trusted (#139).
	if err := rowseal.Check(ctx, rowseal.Users, userRow); err != nil {
		return "", fmt.Errorf("%w: %w", ErrAccountUnverified, err)
	}
	if err := rowseal.Check(ctx, rowseal.Sessions, sessionRow); err != nil {
		return "", fmt.Errorf("%w: %w", ErrSessionUnverified, err)
	}
	// A freeze ends the sessions, and is reported before them, so that the
	// user learns why their tokens stopped working.
	if frozen {
		return "", ErrAccountFrozen
	}
	if !active {
		return "", ErrSessionInvalid
	}
	if deleted || role != RoleUser || !account.UserStatus(status).CanSignIn() {
		return "", ErrAccountUnavailable
	}
	return locale, nil
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
		if err := rowseal.Reseal(ctx, tx, rowseal.Sessions, "id = $1 AND user_id = $2", sessionID, userID); err != nil {
			return nil, false, err
		}
		return &event, true, nil
	})
}

func (s *PostgresSessionStore) Note(ctx context.Context, event audit.Event) error {
	return audit.Record(ctx, s.DB, event)
}
