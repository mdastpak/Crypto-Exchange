// Package audit records who did what, when, from where, and with which
// outcome (#51), in the append-only audit_events table.
//
// An event of a change is written in the transaction of the change, so that
// it commits, or rolls back, with it: a change cannot happen without its
// event. An event of an attempt that changes nothing, such as a failed
// sign-in, is written on its own.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Actor types.
const (
	ActorUser      = "user"
	ActorAdmin     = "admin"
	ActorAnonymous = "anonymous"
	ActorSystem    = "system"
)

// Outcomes.
const (
	Success = "success"
	// Failure is an attempt that did not succeed, such as a wrong code.
	Failure = "failure"
	// Denied is an attempt that was refused before it was tried, such as a
	// rate limit.
	Denied = "denied"
)

// Actions. docs/architecture.md lists them; an action, once recorded, keeps
// its name, so that the log can be searched by it.
const (
	ActionAdminSignInCode = "admin.sign_in_code"
	ActionAdminSignIn     = "admin.sign_in"
	ActionAdminLogout     = "admin.logout"
	// An administrator ended one of their own sessions (#308).
	ActionAdminSessionRevoke = "admin.session_revoke"
	// An administrator gave a fresh authenticator code for the sensitive
	// routes (#308).
	ActionAdminStepUp    = "admin.step_up"
	ActionCurrencyCreate = "currency.create"
	ActionCurrencyUpdate = "currency.update"
	ActionCurrencyDelete = "currency.delete"
	// ActionCurrencyRestore is an administrator undoing a delete (#196).
	ActionCurrencyRestore = "currency.restore"
	// The lifecycle of a currency (#216): an administrator scheduling or
	// cancelling a delisting, or overriding a capability, and the scheduler
	// applying a step.
	ActionCurrencyDelistingSchedule  = "currency.delisting_schedule"
	ActionCurrencyDelistingCancel    = "currency.delisting_cancel"
	ActionCurrencyCapabilityOverride = "currency.capability_override"
	ActionCurrencyLifecycleChange    = "currency.lifecycle_change"
	// ActionCurrencySymbolRelease is an administrator releasing a previous
	// symbol of a currency, for another currency to take (#215).
	ActionCurrencySymbolRelease = "currency.symbol_release"
	// The migrations of currencies into others (#215): an administrator
	// scheduling or cancelling one, and the scheduler applying a phase. The
	// target is the currency that the migration is into.
	ActionCurrencyMigrationSchedule = "currency.migration_schedule"
	ActionCurrencyMigrationCancel   = "currency.migration_cancel"
	ActionCurrencyMigrationPhase    = "currency.migration_phase"
	ActionUserCreate                = "user.create"
	ActionUserSignInCode            = "user.sign_in_code"
	ActionUserSignIn                = "user.sign_in"
	ActionUserRefresh               = "user.token_refresh"
	ActionUserLogout                = "user.logout"
	// ActionUserSessionRevoke is a user ending one of their sessions from
	// the session list (#193); the target is the session.
	ActionUserSessionRevoke = "user.session_revoke"
	// ActionUserProfileUpdate is a user changing their names (#195).
	ActionUserProfileUpdate = "user.profile_update"
	// A user setting the account password, the second step of their
	// sign-in, or changing it with the current one (#307).
	ActionUserPasswordSet    = "user.password_set"
	ActionUserPasswordChange = "user.password_change"
	// ActionUserStepUp is a session proving the second factor again, for
	// the operations that it protects (#307).
	ActionUserStepUp = "user.step_up"
	// The contact changes of a user (#199). The target is the user, and the
	// event holds the contacts masked.
	ActionUserContactAdd     = "user.contact_add"
	ActionUserContactReplace = "user.contact_replace"
	ActionUserContactRemove  = "user.contact_remove"
	// ActionUserContactClaimed is a contact taken off an account because
	// it no longer belongs to its owner (#252): another user proved control
	// of it, or the provider no longer matches the mobile number to the
	// national ID of the account. The target is the account that lost it,
	// and the reason says which.
	ActionUserContactClaimed = "user.contact_claimed"
	// The restrictions of a user by an administrator (#220). The target is
	// the user, the reason is the restriction's, and the event names the
	// restriction.
	ActionUserRestrictionAdd  = "user.restriction_add"
	ActionUserRestrictionLift = "user.restriction_lift"
	// The platform controls (#235): an administrator, or an operator with
	// cmd/admin, creating a maintenance or a lockdown, changing its end or
	// message, asking to lift it when a second administrator must approve,
	// and lifting it. The target is the control.
	ActionPlatformControlCreate      = "platform.control_create"
	ActionPlatformControlUpdate      = "platform.control_update"
	ActionPlatformControlLiftRequest = "platform.control_lift_request"
	ActionPlatformControlLift        = "platform.control_lift"

	// The commands of cmd/admin (#195). Their actor is the system, and the
	// operator who ran them is named in the user agent; see Operator.
	ActionAdminCreate    = "admin.create"
	ActionAdminTOTPReset = "admin.totp_reset"
	ActionAdminDisable   = "admin.disable"
	// An operator ended every session of an administrator (#308).
	ActionAdminSessionsEnd = "admin.sessions_end"
	ActionDataKeyCreate    = "data_key.create"
	ActionDataKeyRotate    = "data_key.rotate"
	ActionDataKeyRetire    = "data_key.retire"
	ActionDataKeyRewrap    = "data_key.rewrap"
	ActionOutboxRetry      = "outbox.retry"
	ActionOutboxDiscard    = "outbox.discard"

	// The administration of the market data catalog (#270), by an
	// administrator or cmd/admin market-data. Pausing or resuming a source,
	// enabling or disabling a venue, hiding or showing an asset, linking it
	// to a listed currency or unlinking it, setting or clearing the tier of
	// an asset or a pair, mapping a pair to the assets of the catalog,
	// resolving a conflict of identity, and running a job now.
	ActionMarketSourcePause     = "market_data.source_pause"
	ActionMarketSourceResume    = "market_data.source_resume"
	ActionMarketVenueEnable     = "market_data.venue_enable"
	ActionMarketVenueDisable    = "market_data.venue_disable"
	ActionMarketAssetHide       = "market_data.asset_hide"
	ActionMarketAssetUnhide     = "market_data.asset_unhide"
	ActionMarketAssetLink       = "market_data.asset_link"
	ActionMarketAssetUnlink     = "market_data.asset_unlink"
	ActionMarketTierOverride    = "market_data.tier_override"
	ActionMarketPairMap         = "market_data.pair_map"
	ActionMarketConflictResolve = "market_data.conflict_resolve"
	ActionMarketJobRun          = "market_data.job_run"
	// The row seals (#139): a row that does not match its seal, found by a
	// read or by the verifier, and an administrator's re-seal of it after
	// review. The target is the table, and the row's primary key.
	ActionIntegritySealMismatch = "integrity.seal_mismatch"
	ActionIntegrityReseal       = "integrity.reseal"
	// An administrator asked for a re-seal, which another approves (#308).
	ActionIntegrityResealRequest = "integrity.reseal_request"
)

// Target types.
const (
	TargetCurrency = "currency"
	TargetUser     = "user"
	// TargetSession is a user's session, by the session ID of its tokens.
	TargetSession = "session"
	// TargetAdminSession is an administrator's session (#308), by the
	// session ID of its token.
	TargetAdminSession = "admin_session"
	// TargetSignIn is a sign-in in progress, by the ID that a code request
	// returns, which links the code request to the attempts to verify it.
	TargetSignIn = "sign_in"
	// TargetDataKey is a data-protection key, as purpose/vN.
	TargetDataKey = "data_key"
	// TargetOutboxMessage is a message of the outbox, by its id.
	TargetOutboxMessage = "outbox_message"
	// TargetPlatformControl is a maintenance or a lockdown (#235).
	TargetPlatformControl = "platform_control"
	// The targets of the market data catalog (#270): a source by its name, a
	// venue by its id, an asset, a pair, and a conflict by their ids, and a
	// job by its name.
	TargetMarketSource   = "market_source"
	TargetMarketVenue    = "market_venue"
	TargetMarketAsset    = "market_asset"
	TargetMarketPair     = "market_pair"
	TargetMarketConflict = "market_conflict"
	TargetMarketJob      = "market_job"
)

// Operator returns the origin of the changes that an operator makes with a
// command, such as cmd/admin: the system actor, with the command, the name
// that the operator gave, the operating-system user, and the host in the
// user agent, since a command has no request.
func Operator(command, name, osUser, host string) Event {
	parts := []string{command}
	for _, p := range []struct{ key, value string }{{"operator", name}, {"os_user", osUser}, {"host", host}} {
		if p.value != "" {
			parts = append(parts, p.key+"="+p.value)
		}
	}
	return Event{ActorType: ActorSystem, UserAgent: strings.Join(parts, "; ")}
}

// Of returns a copy of origin for action on a target, with the outcome
// success. An origin without an actor is the system.
func Of(origin Event, action, targetType, targetID string) Event {
	e := origin
	if e.ActorType == "" {
		e.ActorType = ActorSystem
	}
	e.Action, e.Outcome = action, Success
	e.TargetType, e.TargetID = targetType, targetID
	return e
}

// maxUserAgent is the longest user agent kept, in characters.
const maxUserAgent = 512

// nul is the character that PostgreSQL refuses in text.
const nul = "\x00"

// Event is an audit event.
type Event struct {
	ActorType string
	// ActorID is the hk of the user or administrator. It is empty for
	// anonymous and system actors.
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Outcome    string
	// Reason is a stable code of why an action failed or was denied.
	Reason    string
	Changes   *Changes
	RequestID string
	IP        string
	UserAgent string
}

// Changes are the fields of a change, before and after it.
type Changes struct {
	Before map[string]interface{} `json:"before,omitempty"`
	After  map[string]interface{} `json:"after,omitempty"`
}

// Execer runs a statement: a *sql.DB, or the *sql.Tx of the change that an
// event belongs to.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// FromRequest returns an event with the request's ID, client address, and
// user agent. The client address honors server.trusted_proxies.
func FromRequest(c *gin.Context) Event {
	return Event{RequestID: c.GetString("request_id"), IP: c.ClientIP(), UserAgent: c.Request.UserAgent()}
}

// Record writes e. In the transaction of a change, it commits or rolls back
// with the change.
func Record(ctx context.Context, db Execer, e Event) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var changes interface{}
	if e.Changes != nil {
		data, err := json.Marshal(e.Changes)
		if err != nil {
			return fmt.Errorf("audit event %s: %w", e.Action, err)
		}
		changes = string(data)
	}
	ip := ""
	if net.ParseIP(e.IP) != nil {
		ip = e.IP
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO audit_events (hk, actor_type, actor_id, action, target_type, target_id,
			outcome, reason, request_id, ip, user_agent, changes)
		VALUES ($1, $2, NULLIF($3, '')::uuid, $4, NULLIF($5, ''), NULLIF($6, ''),
			$7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, '')::inet, NULLIF($11, ''), $12::jsonb)`,
		id, e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID,
		e.Outcome, e.Reason, e.RequestID, ip, shorten(e.UserAgent, maxUserAgent), changes)
	if err != nil {
		return fmt.Errorf("record the audit event %s: %w", e.Action, err)
	}
	return nil
}

// shorten cuts s to at most n characters. It replaces invalid UTF-8
// and drops NUL characters, which PostgreSQL refuses in text, since a client
// chooses its user agent.
func shorten(s string, n int) string {
	if !utf8.ValidString(s) {
		s = string([]rune(s))
	}
	s = strings.ReplaceAll(s, nul, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
