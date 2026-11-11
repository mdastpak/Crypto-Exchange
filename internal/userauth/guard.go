package userauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// The modes of security.second_factor.mode: when a user must have a second
// factor.
const (
	// ModeOptional leaves the second factor to the user. Those who set
	// one must still prove it for the operations it protects.
	ModeOptional = config.SecondFactorOptional
	// ModeRequiredForFunds refuses deposits, withdrawals, and the other
	// operations of funds until the user has set one.
	ModeRequiredForFunds = config.SecondFactorRequiredForFunds
)

// The codes of a refusal by the guard.
const (
	// CodeSecondFactorRequired: the user must set a second factor first.
	CodeSecondFactorRequired = "second_factor_required"
	// CodeStepUpRequired: the session must prove the second factor again.
	CodeStepUpRequired = "step_up_required"
)

// Refusal is an operation that the guard refuses. Code is one of the codes
// above.
type Refusal struct{ Code string }

func (r *Refusal) Error() string { return "the second factor refuses the operation: " + r.Code }

// Querier reads: a *sql.DB, or the *sql.Tx of an operation.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Guard decides whether a session may run an operation that the second
// factor protects. Its state is in PostgreSQL, so every instance agrees.
type Guard struct {
	DB *sql.DB
}

// mode returns security.second_factor.mode, or its default.
func mode() string {
	if m := config.Config.Security.SecondFactor.Mode; m != "" {
		return m
	}
	return config.DefaultSecondFactorMode
}

// StepUpLifetime returns security.second_factor.step_up_minutes, or its
// default, as a duration.
func StepUpLifetime() time.Duration {
	minutes := config.Config.Security.SecondFactor.StepUpMinutes
	if minutes == 0 {
		minutes = config.DefaultUserStepUpMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// State is where a session stands with the second factor.
type State struct {
	// Mode is security.second_factor.mode.
	Mode string `json:"mode" enums:"optional,required_for_funds" example:"required_for_funds"`
	// Enrolled reports whether the user has a second factor.
	Enrolled bool `json:"enrolled"`
	// ElevatedUntil is until when the session may run the protected
	// operations without proving the factor again. It is left out when
	// the session is not elevated.
	ElevatedUntil *time.Time `json:"elevated_until,omitempty"`
}

// State reads the state of a session of a user.
func (g *Guard) State(ctx context.Context, q Querier, userHK, sessionID string) (State, error) {
	s := State{Mode: mode()}
	var until sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM user_passwords WHERE user_hk = $1),
			(SELECT elevated_until FROM session_step_ups WHERE session_id = $2 AND elevated_until > NOW())`,
		userHK, sessionID).Scan(&s.Enrolled, &until)
	if err != nil {
		return State{}, fmt.Errorf("read the second factor of the session: %w", err)
	}
	if until.Valid {
		s.ElevatedUntil = &until.Time
	}
	return s, nil
}

// Check returns nil when the session may run a protected operation now,
// and a *Refusal otherwise. funds says whether the operation moves funds,
// such as a deposit or a withdrawal: in the mode required_for_funds, those
// need a second factor, so a user without one is refused until they set
// it. Any other protected operation is open to a user without a factor.
//
// A path that changes money calls it in the transaction of the change,
// with the tx as q.
func (g *Guard) Check(ctx context.Context, q Querier, userHK, sessionID string, funds bool) error {
	state, err := g.State(ctx, q, userHK, sessionID)
	if err != nil {
		return err
	}
	switch {
	case !state.Enrolled && funds && state.Mode == ModeRequiredForFunds:
		return &Refusal{Code: CodeSecondFactorRequired}
	case !state.Enrolled:
		return nil
	case state.ElevatedUntil == nil:
		return &Refusal{Code: CodeStepUpRequired}
	}
	return nil
}

// Elevate lets a session run the protected operations for
// StepUpLifetime from now, after it proved the second factor, and returns
// until when. It needs an active session of the user.
func (g *Guard) Elevate(ctx context.Context, userHK, sessionID string) (time.Time, error) {
	var until time.Time
	err := g.DB.QueryRowContext(ctx, `
		INSERT INTO session_step_ups (session_id, elevated_until)
		SELECT id, NOW() + make_interval(secs => $3) FROM sessions
		WHERE id = $2 AND user_id = $1 AND status = 'ACTIVE' AND revoked_at IS NULL
		ON CONFLICT (session_id) DO UPDATE SET elevated_until = EXCLUDED.elevated_until, updated_at = NOW()
		RETURNING elevated_until`, userHK, sessionID, StepUpLifetime().Seconds()).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, errors.New("the session to elevate is not an active session of the user")
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("elevate the session: %w", err)
	}
	return until, nil
}
