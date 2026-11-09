package platform

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// Messages of the refusals, for clients that show no message of their own.
var (
	messageMaintenance  = i18n.M("MAINTENANCE")
	messageLocked       = i18n.M("FEATURE_LOCKED")
	messageStateUnknown = i18n.M("FEATURE_STATE_UNKNOWN")
)

// LockedInfo is the info of a refusal by a control.
type LockedInfo struct {
	Kind Kind `json:"kind" enums:"maintenance,lockdown"`
	// Feature is the closed feature, or, for a maintenance, the use that
	// was refused.
	Feature Feature    `json:"feature"`
	EndsAt  *time.Time `json:"ends_at,omitempty"`
	Message Message    `json:"message"`
	// StateUnknown is a refusal because the instance could not read the
	// controls; it ends once it can.
	StateUnknown bool `json:"state_unknown,omitempty"`
}

// Send answers a refusal with 503, its code, and Retry-After when the
// control has an end. It reports false, and sends nothing, for an err that
// is not a *Locked.
func Send(c *gin.Context, err error) bool {
	var l *Locked
	if !errors.As(err, &l) {
		return false
	}
	message := messageLocked
	switch {
	case l.Kind == KindMaintenance:
		message = messageMaintenance
	case l.StateUnknown:
		message = messageStateUnknown
		c.Header("Retry-After", "5")
	}
	if wait := l.RetryAfter(time.Now()); wait > 0 {
		c.Header("Retry-After", strconv.Itoa(int(wait/time.Second)))
	}
	info := LockedInfo{Kind: l.Kind, Feature: l.Feature, Message: l.Message, StateUnknown: l.StateUnknown}
	if l.EndsAt != nil {
		end := l.EndsAt.UTC()
		info.EndsAt = &end
	}
	helpers.SendError(c, http.StatusServiceUnavailable, l.Code(), message, info)
	return true
}

// SubjectOf returns who sends a request: the user of its access token, once
// the authentication middleware has run, and the client address, which
// honors server.trusted_proxies.
func SubjectOf(c *gin.Context) Subject {
	s := Subject{IP: c.ClientIP()}
	if claims, ok := auth.GetClaims(c); ok && claims.Role == auth.RoleUser {
		s.UserHK = claims.UUID
	}
	return s
}

// Gate refuses the requests whose use a control closes. A nil Gate lets
// every request through, for tests and tools that run without controls.
type Gate struct {
	Watcher *Watcher
}

// Require refuses a request when a control closes any of uses for it.
func (g *Gate) Require(uses ...Feature) gin.HandlerFunc {
	return func(c *gin.Context) {
		if g == nil || g.Watcher == nil {
			c.Next()
			return
		}
		subject := SubjectOf(c)
		for _, use := range uses {
			if locked := g.Watcher.Locked(use, subject); locked != nil {
				Send(c, locked)
				return
			}
		}
		c.Next()
	}
}

// Writes refuses the changes of users, every method but GET, HEAD, and
// OPTIONS, while a read-only lockdown or a maintenance applies.
func (g *Gate) Writes() gin.HandlerFunc {
	require := g.Require(UseWrites)
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
		default:
			require(c)
		}
	}
}

// Locked returns what closes use for subject now, or nil, for the gates
// that are not middleware, such as the WebSocket upgrade.
func (g *Gate) Locked(use Feature, subject Subject) *Locked {
	if g == nil || g.Watcher == nil {
		return nil
	}
	return g.Watcher.Locked(use, subject)
}
