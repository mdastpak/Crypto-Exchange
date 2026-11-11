package public

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/totp"
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
	methodCodeTOTP     = "OTP+TOTP"
	methodCodePasskey  = "OTP+PASSKEY"
	// factorPassword and factorTOTP name the account password and the
	// authenticator app among the second steps.
	factorPassword = "password"
	factorTOTP     = "totp"
	factorPasskey  = "passkey"
	// provedMethodKey holds, in the request context, the session method
	// of a second step that was proved with something else than the
	// password.
	provedMethodKey = "sign_in_proved_method"
	// proofRecoveryCode names the recovery codes among the proofs of a
	// password reset.
	proofRecoveryCode = "recovery_code"

	codeSecondStepRequired = "second_step_required"
	codePasswordInvalid    = "password_invalid"
	codePasswordLocked     = "password_locked"
	codeTOTPInvalid        = "totp_invalid"
	codeTOTPLocked         = "totp_locked"
	codePasskeyInvalid     = "passkey_invalid"
	codePasskeyUnavailable = "passkey_unavailable"
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
	Methods []string `json:"methods" example:"password,totp"`
	// PasswordReset are the proofs with which this account can reset a
	// forgotten password at POST /public/auth/password-reset, besides the
	// code that was just checked. Empty: none is left, and the account
	// needs account recovery.
	PasswordReset []string `json:"password_reset" example:"recovery_code,totp"`
	// Recovery is the account recovery in progress, if any. The right
	// password cancels it.
	Recovery *userauth.Recovery `json:"recovery,omitempty"`
}

// challenged is a second-step challenge that is valid.
type challenged struct {
	UserHK uuid.UUID
	// ContactID is the contact whose code the challenge answered, and 0
	// when it is not a verified contact of the account.
	ContactID int64
	// Key is the Redis key of the challenge.
	Key string
	// Attempt is the audit event of a refusal.
	Attempt audit.Event
}

// SecondStepRequest finishes a sign-in with the account password.
type SecondStepRequest struct {
	Challenge string `json:"challenge"`
	// Password is the account password. Exactly one of Password and
	// TOTPCode is sent.
	Password string `json:"password,omitempty"`
	// TOTPCode is a code of the authenticator app, for an account whose
	// methods list "totp".
	TOTPCode string `json:"totp_code,omitempty" example:"123456"`
	// Passkey is the answer of navigator.credentials.get to the options
	// of POST /public/auth/second-step/passkey, for an account whose
	// methods list "passkey".
	Passkey json.RawMessage `json:"passkey,omitempty" swaggertype:"object"`
}

// PasskeyOptionsRequest asks for the options of a passkey proof.
type PasskeyOptionsRequest struct {
	Challenge string `json:"challenge"`
}

// signInScope names the sign-in of a challenge to the passkeys: a proof
// that began for one challenge finishes no other.
func signInScope(challenge string) string { return "sign_in:" + challenge }

// hasPasskey reports whether the user can prove the second step with a
// passkey. A failure to read it reports false: the password still signs in.
func (h *PublicHandler) hasPasskey(ctx context.Context, userHK uuid.UUID) bool {
	if h.Passkeys == nil {
		return false
	}
	n, err := h.Passkeys.Count(ctx, userHK.String())
	if err != nil {
		log.Printf("Failed to read the passkeys of a sign-in: %v", err)
		return false
	}
	return n > 0
}

// verifyPasskey checks the passkey answer of a challenge. It answers, and
// reports false, when the answer does not prove the user.
func (h *PublicHandler) verifyPasskey(c *gin.Context, ctx context.Context, userHK uuid.UUID, challenge string, response []byte, attempt audit.Event) bool {
	if h.Passkeys == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return false
	}
	err := h.Passkeys.FinishAssertion(ctx, userHK.String(), signInScope(challenge), response)
	switch {
	case errors.Is(err, passkey.ErrDisabled):
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return false
	case errors.Is(err, passkey.ErrInvalid), errors.Is(err, passkey.ErrNoCeremony), errors.Is(err, passkey.ErrCloned), errors.Is(err, passkey.ErrNoPasskey):
		reason := codePasskeyInvalid
		if errors.Is(err, passkey.ErrCloned) {
			reason = "passkey_cloned"
		}
		h.noteSignIn(ctx, attempt, audit.Failure, reason)
		helpers.SendError(c, http.StatusUnauthorized, codePasskeyInvalid, i18n.M("PASSKEY_INVALID"), nil)
		return false
	case err != nil:
		log.Printf("Failed to check the passkey of a sign-in: %v", err)
		h.noteSignIn(ctx, attempt, audit.Failure, "passkey_check_failed")
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return false
	}
	return true
}

// @Summary Get the options of a passkey proof for a sign-in
// @Description For an account whose methods list passkey: answers the options for navigator.credentials.get, bound to the challenge of the sign-in. The answer of the device goes, as passkey, to POST /public/auth/second-step or POST /public/auth/password-reset within five minutes, once. The device must verify its holder, with a biometric or a PIN.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body PasskeyOptionsRequest true "The challenge"
// @Success 200 {object} models.APIResponse{info=passkey.Options} "The options, under publicKey"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid"
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse "passkey_not_found: the account has no passkey"
// @Failure 503 {object} models.APIResponse "passkey_unavailable: passkeys are not configured"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/second-step/passkey [post]
func (h *PublicHandler) SecondStepPasskeyOptions(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	var req PasskeyOptionsRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Challenge == "" || len(req.Challenge) > 128 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"challenge": "is required"}})
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil || h.Passkeys == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return
	}
	userHK, _, _, ok := h.challengedUser(c, ctx, req.Challenge)
	if !ok {
		return
	}
	options, err := h.Passkeys.BeginAssertion(ctx, userHK.String(), signInScope(req.Challenge))
	switch {
	case errors.Is(err, passkey.ErrDisabled):
		helpers.SendError(c, http.StatusServiceUnavailable, codePasskeyUnavailable, i18n.M("PASSKEYS_UNAVAILABLE"), nil)
		return
	case errors.Is(err, passkey.ErrNoPasskey):
		helpers.SendError(c, http.StatusConflict, "passkey_not_found", i18n.M("PASSKEY_NOT_FOUND"), nil)
		return
	case err != nil:
		log.Printf("Failed to begin the passkey proof of a sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PASSKEY_OPTIONS"), options)
}

// hasTOTP reports whether the user can prove the second step with an
// authenticator app. A failure to read it reports false: the password
// still signs in.
func (h *PublicHandler) hasTOTP(ctx context.Context, userHK uuid.UUID) bool {
	if h.TOTP == nil {
		return false
	}
	enabled, err := h.TOTP.Enabled(ctx, userHK.String())
	if err != nil {
		log.Printf("Failed to read the authenticator of a sign-in: %v", err)
		return false
	}
	return enabled
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
// The challenge names the contact whose code was right, which an account
// recovery counts as proved.
func (h *PublicHandler) askSecondStep(c *gin.Context, ctx context.Context, userHK uuid.UUID, contactType, contact string, attempt audit.Event) {
	if h.RedisClient == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	var contactID int64
	err := h.PostgresDB.DB.QueryRowContext(ctx, `SELECT c.id FROM user_contacts c
		WHERE c.user_hk = $3 AND c.verified_at IS NOT NULL AND `+db.ContactMatch, contactType, contact, userHK).Scan(&contactID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("Failed to read the contact of a sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails since Go 1.24
	challenge := base64.RawURLEncoding.EncodeToString(raw)
	if err := h.RedisClient.Client.Set(ctx, secondStepKey(challenge), userHK.String()+"|"+strconv.FormatInt(contactID, 10), secondStepTTL).Err(); err != nil {
		// Without the challenge the sign-in cannot continue, and it never
		// continues without the second step.
		log.Printf("Failed to store a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return
	}
	reset := []string{}
	if left, err := h.Passwords.RecoveryCodesLeft(ctx, h.PostgresDB.DB, userHK.String()); err != nil {
		log.Printf("Failed to count the recovery codes of a sign-in: %v", err)
	} else if left > 0 {
		reset = append(reset, proofRecoveryCode)
	}
	recovery, err := h.Passwords.OpenRecovery(ctx, userHK.String())
	if err != nil {
		log.Printf("Failed to read the recovery of a sign-in: %v", err)
	}
	methods := []string{factorPassword}
	if h.hasTOTP(ctx, userHK) {
		methods = append(methods, factorTOTP)
		reset = append(reset, factorTOTP)
	}
	if h.hasPasskey(ctx, userHK) {
		methods = append(methods, factorPasskey)
		reset = append(reset, factorPasskey)
	}
	h.noteSignIn(ctx, attempt, audit.Success, codeSecondStepRequired)
	helpers.SendResponse(c, http.StatusAccepted, i18n.M("SECOND_STEP_REQUIRED"), SecondStepInfo{
		Recovery:           recovery,
		SecondStepRequired: true,
		Challenge:          challenge,
		ExpiresIn:          int(secondStepTTL.Seconds()),
		Methods:            methods,
		PasswordReset:      reset,
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

// challengedUser returns the user of a second-step challenge, which stays
// usable, with its Redis key and the audit event of a refusal. It answers,
// and reports false, when the challenge is not valid or the account may no
// longer sign in.
func (h *PublicHandler) challengedUser(c *gin.Context, ctx context.Context, challenge string) (uuid.UUID, string, audit.Event, bool) {
	ch, ok := h.challenge(c, ctx, challenge)
	return ch.UserHK, ch.Key, ch.Attempt, ok
}

// challenge is challengedUser with the contact of the challenge.
func (h *PublicHandler) challenge(c *gin.Context, ctx context.Context, challenge string) (challenged, bool) {
	key := secondStepKey(challenge)
	value, err := h.RedisClient.Client.Get(ctx, key).Result()
	owner, contactPart, _ := strings.Cut(value, "|")
	userHK, parseErr := uuid.Parse(owner)
	contactID, _ := strconv.ParseInt(contactPart, 10, 64)
	if errors.Is(err, redis.Nil) || (err == nil && parseErr != nil) {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return challenged{}, false
	}
	if err != nil {
		log.Printf("Failed to read a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return challenged{}, false
	}

	attempt := audit.FromRequest(c)
	attempt.Action, attempt.ActorType, attempt.ActorID = audit.ActionUserSignIn, audit.ActorUser, userHK.String()
	attempt.TargetType, attempt.TargetID = audit.TargetUser, userHK.String()

	allowed, frozen, err := h.canSignIn(ctx, userHK)
	switch {
	case err != nil:
		log.Printf("Failed to read the user of a second-step challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return challenged{}, false
	case frozen:
		h.noteSignIn(ctx, attempt, audit.Denied, restrictions.CodeAccountFrozen)
		helpers.SendError(c, http.StatusForbidden, restrictions.CodeAccountFrozen, restrictions.MessageAccountFrozen, nil)
		return challenged{}, false
	case !allowed:
		h.noteSignIn(ctx, attempt, audit.Failure, helpers.CodeAccountUnavailable)
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return challenged{}, false
	}

	return challenged{UserHK: userHK, ContactID: contactID, Key: key, Attempt: attempt}, true
}

// @Summary Finish a sign-in with the account password
// @Description After a correct code answered 202 with a challenge, the account password signs in, or a code of the authenticator app (totp_code) for an account whose methods list totp: 401 totp_invalid for a wrong or used code, 429 totp_locked after too many. An account whose methods list passkey can send, as passkey, the answer of the device to the options of POST /public/auth/second-step/passkey: 401 passkey_invalid when it proves nothing. The challenge works for five minutes, until a sign-in succeeds with it. A wrong password answers 401 password_invalid and leaves the challenge usable; after security.password.max_failures wrong passwords in a row the password is locked for security.password.lock_minutes, on every instance, and the answer is 429 password_locked with Retry-After.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body SecondStepRequest true "The challenge and the password"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo} "The access token. The refresh token is set as an HttpOnly cookie."
// @Header 200 {string} Set-Cookie "The refresh token, HttpOnly"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid, password_invalid, totp_invalid, or passkey_invalid"
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
	byTOTP, byPasskey := req.TOTPCode != "", len(req.Passkey) > 0
	switch {
	case byPasskey && (byTOTP || req.Password != ""):
		errs["passkey"] = "cannot be sent with password or totp_code"
	case byPasskey:
	case byTOTP && req.Password != "":
		errs["totp_code"] = "cannot be sent with password"
	case byTOTP:
		var ok bool
		if req.TOTPCode, ok = contact.Code(req.TOTPCode, totp.Digits); !ok {
			errs["totp_code"] = "must be " + strconv.Itoa(totp.Digits) + " digits"
		}
	case req.Password == "" || len(req.Password) > userauth.MaxPasswordBytes:
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

	userHK, key, attempt, ok := h.challengedUser(c, ctx, req.Challenge)
	if !ok {
		return
	}

	if byPasskey {
		if !h.verifyPasskey(c, ctx, userHK, req.Challenge, req.Passkey, attempt) {
			return
		}
		c.Set(provedMethodKey, methodCodePasskey)
		h.finishSecondStep(c, ctx, key, userHK, attempt)
		return
	}
	if byTOTP {
		if !h.verifyTOTP(c, ctx, userHK, req.TOTPCode, attempt) {
			return
		}
		c.Set(provedMethodKey, methodCodeTOTP)
		h.finishSecondStep(c, ctx, key, userHK, attempt)
		return
	}

	err := h.Passwords.Verify(ctx, userHK.String(), req.Password)
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

	h.finishSecondStep(c, ctx, key, userHK, attempt)
}

// verifyTOTP checks an authenticator code of a challenge. It answers, and
// reports false, when the code does not prove the user.
func (h *PublicHandler) verifyTOTP(c *gin.Context, ctx context.Context, userHK uuid.UUID, code string, attempt audit.Event) bool {
	if h.TOTP == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return false
	}
	err := h.TOTP.Verify(ctx, userHK.String(), code)
	var locked *userauth.LockedError
	switch {
	case errors.As(err, &locked):
		h.noteSignIn(ctx, attempt, audit.Denied, codeTOTPLocked)
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(locked.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codeTOTPLocked, i18n.M("TOTP_LOCKED"), nil)
		return false
	case errors.Is(err, userauth.ErrWrongTOTP), errors.Is(err, userauth.ErrNoTOTP):
		// An account without an authenticator answers like a wrong code.
		h.noteSignIn(ctx, attempt, audit.Failure, codeTOTPInvalid)
		helpers.SendError(c, http.StatusUnauthorized, codeTOTPInvalid, i18n.M("TOTP_INVALID"), nil)
		return false
	case err != nil:
		log.Printf("Failed to check the authenticator code of a sign-in: %v", err)
		h.noteSignIn(ctx, attempt, audit.Failure, "totp_check_failed")
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_SESSION"), nil)
		return false
	}
	return true
}

// finishSecondStep spends the challenge of a proved second step and signs in.
func (h *PublicHandler) finishSecondStep(c *gin.Context, ctx context.Context, key string, userHK uuid.UUID, attempt audit.Event) {
	// The challenge signs in once: of two requests with the right proof,
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
