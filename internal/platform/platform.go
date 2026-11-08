// Package platform holds the controls that take the whole service, or parts
// of it, out of users' reach (#235): a full maintenance, and lockdowns of
// single features such as withdrawals.
//
// The platform_controls table is the only source of truth, so that every
// instance applies the same controls. A control applies from its start
// until it is lifted or reaches its end, so a scheduled control needs no
// job. Each instance keeps a copy in memory (Watcher), which the HTTP and
// WebSocket gates read without a query, and refreshes it when the version
// in platform_state changes. An operation that changes money checks the
// table itself, in its transaction (Check), so that an operation in flight
// when a control is created cannot commit after it.
package platform

import (
	"fmt"
	"net"
	"slices"
	"time"
)

// Kind is what a control does.
type Kind string

const (
	// KindMaintenance closes the whole service to users. Administrators,
	// the status endpoint, the probes, and sign-out keep working.
	KindMaintenance Kind = "maintenance"
	// KindLockdown closes one feature.
	KindLockdown Kind = "lockdown"
)

// Feature is a part of the service that a lockdown can close.
type Feature string

// The features that a lockdown can close.
const (
	// FeatureSignUp refuses new accounts; existing users still sign in.
	FeatureSignUp Feature = "sign_up"
	// FeatureSignIn refuses new sign-ins; existing sessions continue.
	FeatureSignIn Feature = "sign_in"
	// FeatureProfileChanges refuses changes of names and contacts.
	FeatureProfileChanges Feature = "profile_changes"
	// FeatureDeposits, FeatureWithdrawals, and FeatureTrading close the
	// money flows, which check them in their transaction.
	FeatureDeposits    Feature = "deposits"
	FeatureWithdrawals Feature = "withdrawals"
	FeatureTrading     Feature = "trading"
	// FeatureAPIKeys refuses requests signed with API keys (#64); sessions
	// of the web and mobile apps continue.
	FeatureAPIKeys Feature = "api_keys"
	// FeatureWebSocket refuses new connections and closes open ones.
	FeatureWebSocket Feature = "websocket"
	// FeatureReadOnly refuses every change by users; reads continue.
	FeatureReadOnly Feature = "read_only"
	// FeatureScheduler pauses the scheduler of currency delistings and
	// migrations, and FeatureOutbox the relay of the outbox, for
	// maintenance that needs a quiet database. Users never see them.
	FeatureScheduler Feature = "scheduler"
	FeatureOutbox    Feature = "outbox"
)

// Features are the features that a lockdown can close.
var Features = []Feature{
	FeatureSignUp, FeatureSignIn, FeatureProfileChanges, FeatureDeposits, FeatureWithdrawals,
	FeatureTrading, FeatureAPIKeys, FeatureWebSocket, FeatureReadOnly, FeatureScheduler, FeatureOutbox,
}

// Uses of the service that no lockdown names, which the gates check.
const (
	// UseService is any request of a user. Only a full maintenance closes
	// it.
	UseService Feature = "service"
	// UseWrites is a change by a user that no feature names, such as a
	// favorite. A maintenance and a read-only lockdown close it.
	UseWrites Feature = "writes"
)

// Valid reports whether f is a feature that a lockdown can close.
func (f Feature) Valid() bool { return slices.Contains(Features, f) }

// internal reports whether users never see a lockdown of f.
func (f Feature) internal() bool { return f == FeatureScheduler || f == FeatureOutbox }

// covers reports whether a control of kind and feature closes use. A full
// maintenance closes every use of users, but not the background work. A
// read-only lockdown closes every change.
func covers(kind Kind, feature Feature, use Feature) bool {
	if kind == KindMaintenance {
		return !use.internal()
	}
	if feature == use {
		return true
	}
	if feature == FeatureReadOnly {
		switch use {
		case UseWrites, FeatureSignUp, FeatureProfileChanges, FeatureDeposits, FeatureWithdrawals, FeatureTrading:
			return true
		}
	}
	return false
}

// Message is a text for users, in Persian and English.
type Message struct {
	FA string `json:"fa,omitempty"`
	EN string `json:"en,omitempty"`
}

// Control is a maintenance or a lockdown.
type Control struct {
	HK      string   `json:"id" format:"uuid"`
	Kind    Kind     `json:"kind" enums:"maintenance,lockdown"`
	Feature *Feature `json:"feature,omitempty" enums:"sign_up,sign_in,profile_changes,deposits,withdrawals,trading,api_keys,websocket,read_only,scheduler,outbox"`
	// StartsAt is when the control applies; EndsAt, when set, when it
	// stops.
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	// AnnounceAt is when users learn of the control.
	AnnounceAt time.Time `json:"announce_at"`
	Message    Message   `json:"message"`
	// Reason is internal: users never see it.
	Reason string `json:"reason"`
	// AllowNetworks and AllowUsers may still use the service during a
	// maintenance.
	AllowNetworks []string `json:"allow_networks"`
	AllowUsers    []string `json:"allow_users"`
	// CreatedBy, LiftRequestedBy, and LiftedBy are administrators; they
	// are absent for a change made with cmd/admin.
	CreatedBy       *string    `json:"created_by,omitempty" format:"uuid"`
	CreatedAt       time.Time  `json:"created_at"`
	LiftRequestedBy *string    `json:"lift_requested_by,omitempty" format:"uuid"`
	LiftRequestedAt *time.Time `json:"lift_requested_at,omitempty"`
	LiftedAt        *time.Time `json:"lifted_at,omitempty"`
	LiftedBy        *string    `json:"lifted_by,omitempty" format:"uuid"`
	LiftNote        string     `json:"lift_note,omitempty"`
	Version         int64      `json:"version" example:"1"`
	// Active is whether the control applies now.
	Active bool `json:"active"`

	// networks are AllowNetworks, parsed.
	networks []*net.IPNet
}

// activeAt reports whether c applies at now.
func (c *Control) activeAt(now time.Time) bool {
	return c.LiftedAt == nil && !now.Before(c.StartsAt) && (c.EndsAt == nil || now.Before(*c.EndsAt))
}

// endedAt reports whether c no longer applies, and never will again.
func (c *Control) endedAt(now time.Time) bool {
	return c.LiftedAt != nil || (c.EndsAt != nil && !now.Before(*c.EndsAt))
}

// feature returns the feature of c, empty for a maintenance.
func (c *Control) feature() Feature {
	if c.Feature == nil {
		return ""
	}
	return *c.Feature
}

// visible reports whether users see c.
func (c *Control) visible() bool { return !c.feature().internal() }

// allows reports whether a maintenance lets subject through.
func (c *Control) allows(s Subject) bool {
	if c.Kind != KindMaintenance {
		return false
	}
	if s.UserHK != "" && slices.Contains(c.AllowUsers, s.UserHK) {
		return true
	}
	if ip := net.ParseIP(s.IP); ip != nil {
		for _, n := range c.networks {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// parseNetworks fills c.networks from c.AllowNetworks.
func (c *Control) parseNetworks() error {
	c.networks = c.networks[:0]
	for _, raw := range c.AllowNetworks {
		_, n, err := net.ParseCIDR(raw)
		if err != nil {
			return fmt.Errorf("control %s: allow network %q: %w", c.HK, raw, err)
		}
		c.networks = append(c.networks, n)
	}
	return nil
}

// Subject is who uses the service: the user, when signed in, and the
// client address.
type Subject struct {
	UserHK string
	IP     string
}

// Error codes of the refusals.
const (
	CodeMaintenance   = "maintenance"
	CodeFeatureLocked = "feature_locked"
)

// Locked is a use that a control closes. It is an error.
type Locked struct {
	Kind Kind
	// Feature is the closed feature of a lockdown, the use for a
	// maintenance.
	Feature Feature
	// EndsAt is when the control ends, nil when it has no end.
	EndsAt  *time.Time
	Message Message
	// StateUnknown is a refusal because this instance could not read the
	// controls for longer than platform.stale_after_seconds, of a feature
	// in platform.fail_closed.
	StateUnknown bool
}

// Code returns the error code of l.
func (l *Locked) Code() string {
	if l.Kind == KindMaintenance {
		return CodeMaintenance
	}
	return CodeFeatureLocked
}

func (l *Locked) Error() string {
	if l.Kind == KindMaintenance {
		return "the service is under maintenance"
	}
	return fmt.Sprintf("%s is locked", l.Feature)
}

// RetryAfter returns how long a client should wait, at now, rounded up to a
// second: until the end of the control, or zero when it has none.
func (l *Locked) RetryAfter(now time.Time) time.Duration {
	if l.EndsAt == nil {
		return 0
	}
	d := l.EndsAt.Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d.Truncate(time.Second) + time.Second
}

// evaluate returns what closes use for subject among controls, at now, or
// nil. A maintenance comes before a lockdown. Among controls of the same
// kind, the refusal ends with the last of them, and has no end when one of
// them has none.
func evaluate(controls []*Control, now time.Time, use Feature, subject Subject) *Locked {
	var found *Locked
	for _, c := range controls {
		if !c.activeAt(now) || !covers(c.Kind, c.feature(), use) || c.allows(subject) {
			continue
		}
		feature := c.feature()
		if c.Kind == KindMaintenance {
			feature = use
		}
		switch {
		case found == nil || (c.Kind == KindMaintenance && found.Kind != KindMaintenance):
			found = &Locked{Kind: c.Kind, Feature: feature, EndsAt: c.EndsAt, Message: c.Message}
		case c.Kind != found.Kind:
			// A lockdown after a maintenance changes nothing.
		case c.EndsAt == nil || found.EndsAt == nil:
			found.EndsAt = nil
		case c.EndsAt.After(*found.EndsAt):
			found.EndsAt = c.EndsAt
		}
	}
	return found
}
