package public

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The session limit (#212). A user may have at most jwt.sessions.max_per_user
// active sessions. A sign-in past the limit, with a right code, is refused
// with the user's active sessions and a challenge: a short-lived, single-use
// secret that stands for the verified sign-in. With it, the user ends one of
// the sessions and signs in, in one request and one transaction, without a
// new code.
//
// The challenge lives in Redis, which every instance shares, under the hash
// of its value, and it is taken with GETDEL, so that it is used once even
// when two instances receive it at the same moment. The count of sessions
// runs in the transaction that creates the session, after a lock of the
// user's row, so that concurrent sign-ins on any instance never exceed the
// limit.

const (
	// codeSessionLimit is the error code of a sign-in past the limit.
	codeSessionLimit = "session_limit_reached"
	// sessionLimitChallengeTTL is how long a challenge can be used.
	sessionLimitChallengeTTL = 5 * time.Minute
	// sessionLimitKeyPrefix is the Redis key prefix of the challenges.
	sessionLimitKeyPrefix = "session_limit:"
)

// SessionLimitInfo is the info of a 409 session_limit_reached answer.
type SessionLimitInfo struct {
	// Challenge replaces a session with POST /public/auth/sessions/replace.
	Challenge string `json:"challenge"`
	// ExpiresIn is the number of seconds the challenge can be used.
	ExpiresIn int `json:"expires_in" example:"300"`
	// MaxSessions is the limit.
	MaxSessions int `json:"max_sessions" example:"5"`
	// Sessions are the active sessions of the user, newest first.
	Sessions []auth.SessionInfo `json:"sessions"`
}

// ReplaceSessionRequest ends a session to sign in past the limit.
type ReplaceSessionRequest struct {
	Challenge string `json:"challenge"`
	// SessionID is the session to end, one of the sessions that the
	// refusal listed.
	SessionID string `json:"session_id" format:"uuid"`
}

// maxSessionsPerUser returns jwt.sessions.max_per_user, or its default.
func maxSessionsPerUser() int {
	if n := config.Config.JWT.Sessions.MaxPerUser; n > 0 {
		return n
	}
	return config.DefaultMaxSessionsPerUser
}

// startSession creates a session for a user who proved a sign-in, sets the
// refresh cookie, and answers with the access token. replace, when set, is
// the session that the user ends to sign in past the limit. attempt is the
// audit event of a refusal. It reports whether replace was not one of the
// user's active sessions.
func (h *PublicHandler) startSession(c *gin.Context, ctx context.Context, userHK uuid.UUID, replace string, attempt audit.Event) (replaceNotFound bool) {
	// The method says what the user proved. Every caller comes after the
	// second step of an account that has one.
	method, err := h.sessionMethod(ctx, userHK)
	if err != nil {
		log.Printf("Failed to read the second step of a sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return false
	}
	if proved := c.GetString(provedMethodKey); proved != "" && method != methodCode {
		method = proved
	}
	sessionID := uuid.NewString()
	accessToken, err := auth.GenerateAccessToken(userHK.String(), auth.RoleUser, sessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_GENERATE_ACCESS_TOKEN"), nil)
		return false
	}
	refreshToken, err := auth.GenerateRefreshToken(userHK.String(), auth.RoleUser, sessionID)
	if err != nil {
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_GENERATE_REFRESH_TOKEN"), nil)
		return false
	}

	err = h.Sessions.Create(ctx, auth.Session{
		ID:               sessionID,
		UserID:           userHK.String(),
		AuthMethod:       method,
		AccessTokenHash:  auth.HashToken(accessToken),
		RefreshTokenHash: auth.HashToken(refreshToken),
		UserAgent:        c.Request.UserAgent(),
		IP:               c.ClientIP(),
		ExpiresAt:        time.Now().Add(auth.RefreshTokenLifetime()),
		MaxActive:        maxSessionsPerUser(),
		Replace:          replace,
	}, auth.SessionEvent(c, audit.ActionUserSignIn, userHK.String(), sessionID))
	switch {
	case errors.Is(err, auth.ErrSessionLimit):
		h.noteSignIn(ctx, attempt, audit.Denied, codeSessionLimit)
		h.refuseAtLimit(c, ctx, userHK)
		return false
	case errors.Is(err, auth.ErrAccountFrozen):
		h.noteSignIn(ctx, attempt, audit.Denied, restrictions.CodeAccountFrozen)
		helpers.SendError(c, http.StatusForbidden, restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen, nil)
		return false
	case errors.Is(err, auth.ErrReplacedSessionNotFound):
		helpers.SendError(c, http.StatusConflict, "session_not_found", i18n.M("SESSION_TO_END_NOT_YOURS"), nil)
		return true
	case err != nil:
		// No tokens without the session, nor without its audit event.
		log.Printf("Failed to create session: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return false
	}

	// A sign-in with the second factor is a fresh proof of it. A failure
	// costs the user a step-up, not the sign-in.
	if method != methodCode && h.Guard != nil {
		if _, err := h.Guard.Elevate(ctx, userHK.String(), sessionID); err != nil {
			log.Printf("Failed to elevate the session of a sign-in: %v", err)
		}
	}

	// The refresh token is only delivered in the HttpOnly cookie, never in the
	// body, so scripts running in the page cannot read it.
	auth.SetRefreshTokenCookie(c, refreshToken)

	helpers.SendResponse(c, http.StatusOK, i18n.M("OTP_VERIFIED_SUCCESSFULLY"), gin.H{"access_token": accessToken})
	return false
}

// refuseAtLimit answers a sign-in past the limit with the user's sessions
// and a challenge.
func (h *PublicHandler) refuseAtLimit(c *gin.Context, ctx context.Context, userHK uuid.UUID) {
	lister, ok := h.Sessions.(interface {
		List(ctx context.Context, userID string, page pagination.Request) (auth.SessionPage, error)
	})
	if !ok || h.RedisClient == nil {
		helpers.SendError(c, http.StatusConflict, codeSessionLimit,
			i18n.M("SESSION_LIMIT_REACHED_SIGN_IN_AGAIN"), nil)
		return
	}
	page, err := lister.List(ctx, userHK.String(), pagination.Request{Limit: config.MaxSessionsPerUser})
	if err != nil {
		log.Printf("Failed to list the sessions of a user at the limit: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	challenge, err := h.issueChallenge(ctx, userHK)
	if err != nil {
		log.Printf("Failed to store a session limit challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	helpers.SendError(c, http.StatusConflict, codeSessionLimit,
		i18n.M("SESSION_LIMIT_REACHED"),
		SessionLimitInfo{
			Challenge:   challenge,
			ExpiresIn:   int(sessionLimitChallengeTTL.Seconds()),
			MaxSessions: maxSessionsPerUser(),
			Sessions:    page.Items,
		})
}

func challengeKey(challenge string) string {
	sum := sha256.Sum256([]byte(challenge))
	return sessionLimitKeyPrefix + hex.EncodeToString(sum[:])
}

// issueChallenge stores a new challenge for userHK. Redis holds only its
// hash, so that reading Redis does not give a way to sign in.
func (h *PublicHandler) issueChallenge(ctx context.Context, userHK uuid.UUID) (string, error) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails since Go 1.24
	challenge := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(sessionLimitChallengeTTL).Unix()
	value := userHK.String() + "|" + strconv.FormatInt(expires, 10)
	return challenge, h.RedisClient.Client.Set(ctx, challengeKey(challenge), value, sessionLimitChallengeTTL).Err()
}

// takeChallenge takes a challenge, once, and returns its user and expiry.
func (h *PublicHandler) takeChallenge(ctx context.Context, challenge string) (uuid.UUID, time.Time, bool, error) {
	value, err := h.RedisClient.Client.GetDel(ctx, challengeKey(challenge)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return uuid.Nil, time.Time{}, false, nil
		}
		return uuid.Nil, time.Time{}, false, err
	}
	hk, expiry, found := strings.Cut(value, "|")
	unix, convErr := strconv.ParseInt(expiry, 10, 64)
	userHK, parseErr := uuid.Parse(hk)
	if !found || convErr != nil || parseErr != nil {
		return uuid.Nil, time.Time{}, false, nil
	}
	return userHK, time.Unix(unix, 0), true, nil
}

// putBack stores a taken challenge again until its expiry, for a request
// that could not use it through no fault of the challenge, such as a session
// id that is not the user's.
func (h *PublicHandler) putBack(ctx context.Context, challenge string, userHK uuid.UUID, expires time.Time) {
	ttl := time.Until(expires)
	if ttl <= 0 {
		return
	}
	value := userHK.String() + "|" + strconv.FormatInt(expires.Unix(), 10)
	if err := h.RedisClient.Client.Set(ctx, challengeKey(challenge), value, ttl).Err(); err != nil {
		log.Printf("Failed to restore a session limit challenge: %v", err)
	}
}

// @Summary End a session to sign in past the session limit
// @Description After a sign-in answered 409 session_limit_reached, ends one of the listed sessions and signs in, without a new code. The challenge works once, for five minutes. A session_id that is not one of the user's active sessions answers 409 session_not_found and leaves the challenge usable.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body ReplaceSessionRequest true "The challenge and the session to end"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo} "The access token. The refresh token is set as an HttpOnly cookie."
// @Header 200 {string} Set-Cookie "The refresh token, HttpOnly"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse{info=SessionLimitInfo} "session_not_found, or session_limit_reached with a new challenge"
// @Failure 429 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/sessions/replace [post]
func (h *PublicHandler) ReplaceSession(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var req ReplaceSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		errs["challenge"] = "is required"
	}
	if _, err := uuid.Parse(req.SessionID); err != nil {
		errs["session_id"] = "must be a UUID"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}

	userHK, expires, ok, err := h.takeChallenge(ctx, req.Challenge)
	if err != nil {
		log.Printf("Failed to read a session limit challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	if !ok {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	}

	attempt := audit.FromRequest(c)
	attempt.Action, attempt.ActorType, attempt.ActorID = audit.ActionUserSignIn, audit.ActorUser, userHK.String()
	attempt.TargetType, attempt.TargetID = audit.TargetSession, req.SessionID

	// The account may have been disabled since the code was verified.
	allowed, frozen, err := h.canSignIn(ctx, userHK)
	if err == nil && frozen {
		h.noteSignIn(ctx, attempt, audit.Denied, restrictions.CodeAccountFrozen)
		helpers.SendError(c, http.StatusForbidden, restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen, nil)
		return
	}
	if err == nil && !allowed {
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read the user of a session limit challenge: %v", err)
		h.putBack(ctx, req.Challenge, userHK, expires)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}

	// A session_id that is not the user's leaves the challenge usable, so
	// that the user can pick another session.
	if h.startSession(c, ctx, userHK, req.SessionID, attempt) {
		h.putBack(ctx, req.Challenge, userHK, expires)
	}
}
