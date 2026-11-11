package public

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The second step of a sign-in (#307). A user who has an account password
// gets no token for a correct code alone: the code answers 202 with a
// challenge, and the password, sent with the challenge, signs in. The
// challenge is in Redis, so any instance can finish a sign-in that another
// one started, and it proves only that the code was right.

const (
	// secondStepTTL is how long a challenge can be used.
	secondStepTTL = 5 * time.Minute
	// secondStepKeyPrefix is the Redis key prefix of the challenges.
	secondStepKeyPrefix = "sign_in_second_step:"

	// The session methods: what a user proved to start the session.
	methodCode         = "OTP"
	methodCodePassword = "OTP+PASSWORD"
	// factorPassword names the account password among the second steps.
	factorPassword = "password"

	codeSecondStepRequired = "second_step_required"
	codePasswordInvalid    = "password_invalid"
	codePasswordLocked     = "password_locked"
)

// SecondStepInfo is the info of a 202 answer to a correct code: the sign-in
// is not complete.
type SecondStepInfo struct {
	// SecondStepRequired is always true.
	SecondStepRequired bool `json:"second_step_required" example:"true"`
	// Challenge finishes the sign-in with POST /public/auth/second-step.
	Challenge string `json:"challenge"`
	// ExpiresIn is how long the challenge can be used, in seconds.
	ExpiresIn int `json:"expires_in" example:"300"`
	// Methods are the second steps that the account can use.
	Methods []string `json:"methods" example:"password"`
}

// SecondStepRequest finishes a sign-in with the account password.
type SecondStepRequest struct {
	Challenge string `json:"challenge"`
	Password  string `json:"password"`
}

func secondStepKey(challenge string) string {
	sum := sha256.Sum256([]byte(challenge))
	return secondStepKeyPrefix + hex.EncodeToString(sum[:])
}

// sessionMethod returns what a user must prove to sign in, as the method of
// their session: the code alone, or the code and the account password.
func (h *PublicHandler) sessionMethod(ctx context.Context, userHK uuid.UUID) (string, error) {
	if h.Passwords == nil {
		return methodCode, nil
	}
	has, err := h.Passwords.Has(ctx, userHK.String())
	if err != nil {
		return "", err
	}
	if has {
		return methodCodePassword, nil
	}
	return methodCode, nil
}

// askSecondStep answers a correct code of a user who has a second step with
// a challenge, and no token. Redis holds only the hash of the challenge.
func (h *PublicHandler) askSecondStep(c *gin.Context, ctx context.Context, userHK uuid.UUID, attempt audit.Event) {
	if h.RedisClient == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails since Go 1.24
	challenge := base64.RawURLEncoding.EncodeToString(raw)
	if err := h.RedisClient.Client.Set(ctx, secondStepKey(challenge), userHK.String(), secondStepTTL).Err(); err != nil {
		// Without the challenge the sign-in cannot continue, and it never
		// continues without the second step.
		log.Printf("Failed to store a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	h.noteSignIn(ctx, attempt, audit.Success, codeSecondStepRequired)
	helpers.SendResponse(c, http.StatusAccepted, i18n.M("SECOND_STEP_REQUIRED"), SecondStepInfo{
		SecondStepRequired: true,
		Challenge:          challenge,
		ExpiresIn:          int(secondStepTTL.Seconds()),
		Methods:            []string{factorPassword},
	})
}

// canSignIn reads whether the account of a challenge may still sign in: it
// may have been disabled or frozen since the code was verified.
func (h *PublicHandler) canSignIn(ctx context.Context, userHK uuid.UUID) (allowed, frozen bool, err error) {
	var status, role string
	var deleted bool
	err = h.PostgresDB.DB.QueryRowContext(ctx, `SELECT status, role, deleted_at IS NOT NULL, `+restrictions.FrozenSQL("users.hk")+`
		FROM users WHERE hk = $1`, userHK).Scan(&status, &role, &deleted, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return !deleted && role == auth.RoleUser && account.UserStatus(status).CanSignIn(), frozen, nil
}

// @Summary Finish a sign-in with the account password
// @Description After a correct code answered 202 with a challenge, the account password signs in. The challenge works for five minutes, until a sign-in succeeds with it. A wrong password answers 401 password_invalid and leaves the challenge usable; after security.password.max_failures wrong passwords in a row the password is locked for security.password.lock_minutes, on every instance, and the answer is 429 password_locked with Retry-After.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body SecondStepRequest true "The challenge and the password"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo} "The access token. The refresh token is set as an HttpOnly cookie."
// @Header 200 {string} Set-Cookie "The refresh token, HttpOnly"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid or password_invalid"
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse{info=SessionLimitInfo} "session_limit_reached: the user has the most active sessions allowed"
// @Failure 429 {object} models.APIResponse "password_locked, or a rate limit"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/second-step [post]
func (h *PublicHandler) SecondStep(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req SecondStepRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		errs["challenge"] = "is required"
	}
	if req.Password == "" || len(req.Password) > userauth.MaxPasswordBytes {
		errs["password"] = "is required"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil || h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}

	key := secondStepKey(req.Challenge)
	value, err := h.RedisClient.Client.Get(ctx, key).Result()
	userHK, parseErr := uuid.Parse(value)
	if errors.Is(err, redis.Nil) || (err == nil && parseErr != nil) {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}

	attempt := audit.FromRequest(c)
	attempt.Action, attempt.ActorType, attempt.ActorID = audit.ActionUserSignIn, audit.ActorUser, userHK.String()
	attempt.TargetType, attempt.TargetID = audit.TargetUser, userHK.String()

	allowed, frozen, err := h.canSignIn(ctx, userHK)
	switch {
	case err != nil:
		log.Printf("Failed to read the user of a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	case frozen:
		h.noteSignIn(ctx, attempt, audit.Denied, restrictions.CodeAccountFrozen)
		helpers.SendError(c, http.StatusForbidden, restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen, nil)
		return
	case !allowed:
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	}

	err = h.Passwords.Verify(ctx, userHK.String(), req.Password)
	var locked *userauth.LockedError
	switch {
	case errors.As(err, &locked):
		h.noteSignIn(ctx, attempt, audit.Denied, codePasswordLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codePasswordLocked, i18n.M("PASSWORD_LOCKED"), nil)
		return
	case errors.Is(err, userauth.ErrWrongPassword):
		h.noteSignIn(ctx, attempt, audit.Failure, codePasswordInvalid)
		helpers.SendError(c, http.StatusUnauthorized, codePasswordInvalid, i18n.M("PASSWORD_INVALID"), nil)
		return
	case errors.Is(err, userauth.ErrNoPassword):
		// The challenge was for a password that the account no longer has.
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		// The password was changed outside the backend (#139): it proves
		// nothing, and the account stays closed until an administrator
		// has looked at the finding.
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	case err != nil:
		log.Printf("Failed to check the password of a sign-in: %v", err)
		h.noteSignIn(ctx, attempt, audit.Failure, "password_check_failed")
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}

	// The challenge signs in once: of two requests with the right password,
	// the one that takes it gets the session.
	taken, err := h.RedisClient.Client.Del(ctx, key).Result()
	if err != nil {
		log.Printf("Failed to take a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	if taken == 0 {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	}
	h.startSession(c, ctx, userHK, "", attempt)
}
