package public

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Account recovery (#307): for a user who has neither the account password
// nor a recovery code. Each request carries the challenge of a correct
// code, which proves one contact; the user signs in with each verified
// contact in turn. In the strict mode the owner also proves their identity
// again. A proved recovery waits before it can set a new password, and the
// right password cancels it at any time.

const (
	codeRecoveryIdentity    = "recovery_identity_mismatch"
	codeRecoveryFailed      = "recovery_failed"
	codeRecoveryCooldown    = "recovery_cooldown"
	codeRecoveryNotReady    = "recovery_not_ready"
	codeRecoveryNotFound    = "recovery_not_found"
	codeRecoveryContact     = "recovery_contact_invalid"
	codeRecoveryUnavailable = "recovery_unavailable"
)

// RecoveryRequest starts an account recovery, or adds a proof to it.
type RecoveryRequest struct {
	// Challenge is the challenge that a correct code answered with. Its
	// contact counts as proved.
	Challenge string `json:"challenge"`
	// NationalID is the national ID of the owner, for the step
	// "national_id" of a strict recovery.
	NationalID string `json:"national_id,omitempty" example:"0012345678"`
}

// RecoveryCompleteRequest sets the new password of a recovery.
type RecoveryCompleteRequest struct {
	Challenge   string `json:"challenge"`
	NewPassword string `json:"new_password"`
}

// RecoveryCompleteInfo is the answer to a completed recovery.
type RecoveryCompleteInfo struct {
	// RecoveryCodes are the new recovery codes of the account, shown this
	// once. The earlier ones no longer work.
	RecoveryCodes []string `json:"recovery_codes"`
	// HoldUntil is when the security hold on withdrawals ends.
	HoldUntil time.Time `json:"hold_until"`
}

// recoveryIdentity runs the identity steps of a strict recovery that this
// request can pass. It reports the proof, and false after it answered.
func (h *PublicHandler) recoveryIdentity(c *gin.Context, ctx context.Context, ch challenged, steps []string, givenNationalID, nationalID string) (userauth.RecoveryProof, bool) {
	proof := userauth.RecoveryProof{ContactID: ch.ContactID}
	if slices.Contains(steps, userauth.StepNationalID) && givenNationalID != "" {
		given := contact.Digits(givenNationalID)
		if subtle.ConstantTimeCompare([]byte(given), []byte(nationalID)) != 1 {
			proof.IdentityFailed = true
			return proof, true
		}
		proof.StepsPassed = append(proof.StepsPassed, userauth.StepNationalID)
	}
	if !slices.Contains(steps, userauth.StepMobileOwnership) {
		return proof, true
	}
	var contactType, value string
	err := h.PostgresDB.DB.QueryRowContext(ctx, `SELECT type, value FROM user_contacts
		WHERE id = $1 AND user_hk = $2 AND removed_at IS NULL`, ch.ContactID, ch.UserHK).Scan(&contactType, &value)
	if err != nil {
		// A contact that is gone is refused with the recovery's own answer.
		return proof, true
	}
	if contactType != string(account.ContactMobile) {
		return proof, true
	}
	// The provider says whether the number still belongs to the owner.
	if h.Ownership == nil {
		helpers.SendError(c, http.StatusServiceUnavailable, codeRecoveryUnavailable, i18n.M("RECOVERY_UNAVAILABLE"), nil)
		return proof, false
	}
	checkCtx, cancel := context.WithTimeout(ctx, config.Config.Identity.MobileOwnership.Timeout())
	defer cancel()
	match, err := h.Ownership.Match(checkCtx, nationalID, value)
	if err != nil {
		log.Printf("Failed to check the mobile ownership of a recovery: %v", err)
		helpers.SendError(c, http.StatusServiceUnavailable, codeRecoveryUnavailable, i18n.M("RECOVERY_UNAVAILABLE"), nil)
		return proof, false
	}
	if !match.Matched {
		proof.IdentityFailed = true
		return proof, true
	}
	proof.StepsPassed = append(proof.StepsPassed, userauth.StepMobileOwnership)
	return proof, true
}

// @Summary Start or continue an account recovery
// @Description For a user who has neither the account password nor a recovery code. A correct code answered 202 with a challenge; sent here, it proves the contact of that code. The recovery needs every verified email address and mobile number of the account, so the user signs in with each in turn and sends each challenge here. With security.recovery.mode strict, an account with a verified national ID also passes the steps of security.recovery.steps: national_id, the national ID sent in this request, and mobile_ownership, the provider's confirmation that a proved mobile number belongs to that national ID. A failed identity check answers 401 recovery_identity_mismatch; the third one ends the recovery with 403 recovery_failed, and the account can start another after 24 hours: 429 recovery_cooldown. When everything is proved the recovery is waiting, and after security.recovery.wait_hours POST /public/auth/recovery/complete sets the new password. Proving the account password anywhere cancels the recovery. A recovery has 24 hours to prove everything.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body RecoveryRequest true "The challenge, and the national ID for a strict recovery"
// @Success 200 {object} models.APIResponse{info=userauth.Recovery} "The state of the recovery"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse{info=userauth.Recovery} "challenge_invalid, recovery_contact_invalid, or recovery_identity_mismatch"
// @Failure 403 {object} models.APIResponse "recovery_failed, or the account cannot sign in"
// @Failure 429 {object} models.APIResponse "recovery_cooldown, or a rate limit"
// @Failure 503 {object} models.APIResponse "recovery_unavailable: an identity check cannot run now"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/recovery [post]
func (h *PublicHandler) Recover(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req RecoveryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		errs["challenge"] = "is required"
	}
	if req.NationalID != "" && !identity.ValidNationalID(contact.Digits(req.NationalID)) {
		errs["national_id"] = "is not a valid national ID"
	}
	if len(errs) > 0 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil || h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	ch, ok := h.challenge(c, ctx, req.Challenge)
	if !ok {
		return
	}
	userHK := ch.UserHK.String()

	// The identity of the owner, for a strict recovery. A national ID
	// that cannot be read never turns a strict recovery into a standard
	// one.
	strict := config.Config.Security.Recovery.Mode == config.RecoveryStrict
	var nationalID string
	var hasNationalID bool
	if strict {
		var err error
		nationalID, hasNationalID, err = identity.NationalID(ctx, h.PostgresDB.DB, h.Sealer, userHK)
		if err != nil {
			log.Printf("Failed to read the national ID of a recovery: %v", err)
			helpers.SendError(c, http.StatusServiceUnavailable, codeRecoveryUnavailable, i18n.M("RECOVERY_UNAVAILABLE"), nil)
			return
		}
	}
	open, err := h.Passwords.OpenRecovery(ctx, userHK)
	if err != nil {
		log.Printf("Failed to read the recovery of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECOVER_ACCOUNT"), nil)
		return
	}
	_, steps := userauth.RecoveryPlan(hasNationalID)
	if open != nil {
		steps = open.StepsRequired
	}
	proof := userauth.RecoveryProof{ContactID: ch.ContactID}
	if len(steps) > 0 {
		if !hasNationalID {
			// The recovery started as strict and the identity it would
			// check is gone.
			helpers.SendError(c, http.StatusServiceUnavailable, codeRecoveryUnavailable, i18n.M("RECOVERY_UNAVAILABLE"), nil)
			return
		}
		if proof, ok = h.recoveryIdentity(c, ctx, ch, steps, req.NationalID, nationalID); !ok {
			return
		}
	}

	event := audit.FromRequest(c)
	state, err := h.Passwords.AdvanceRecovery(ctx, userHK, proof, func() (string, []string) {
		return userauth.RecoveryPlan(hasNationalID)
	}, event)
	var cooldown *userauth.RecoveryCooldownError
	switch {
	case errors.Is(err, userauth.ErrRecoveryIdentity):
		helpers.SendError(c, http.StatusUnauthorized, codeRecoveryIdentity, i18n.M("RECOVERY_IDENTITY_MISMATCH"), state)
		return
	case errors.Is(err, userauth.ErrRecoveryFailed):
		helpers.SendError(c, http.StatusForbidden, codeRecoveryFailed, i18n.M("RECOVERY_FAILED"), nil)
		return
	case errors.As(err, &cooldown):
		c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(cooldown.Until).Seconds())+1)))
		helpers.SendError(c, http.StatusTooManyRequests, codeRecoveryCooldown, i18n.M("RECOVERY_COOLDOWN"), nil)
		return
	case errors.Is(err, userauth.ErrRecoveryContact):
		helpers.SendError(c, http.StatusUnauthorized, codeRecoveryContact, i18n.M("RECOVERY_CONTACT_INVALID"), nil)
		return
	case errors.Is(err, userauth.ErrNoPassword):
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	case err != nil:
		log.Printf("Failed to advance the recovery of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECOVER_ACCOUNT"), nil)
		return
	}
	message := i18n.M("RECOVERY_IN_PROGRESS")
	if state.Status == userauth.RecoveryWaiting {
		message = i18n.M("RECOVERY_WAITING")
	}
	helpers.SendResponse(c, http.StatusOK, message, state)
}

// @Summary Set a new password with a recovery
// @Description Completes an account recovery whose wait has ended. The challenge comes from a correct code of a contact that the recovery proved. The new password follows the policy of PUT /user/password. Every session of the user ends, the earlier recovery codes stop working and new ones are returned once, and the security hold starts. The answer does not sign in: the user signs in with a code and the new password. Before the wait ends the answer is 409 recovery_not_ready with Retry-After.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body RecoveryCompleteRequest true "The challenge and the new password"
// @Success 200 {object} models.APIResponse{info=RecoveryCompleteInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid or recovery_contact_invalid"
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse "recovery_not_found"
// @Failure 409 {object} models.APIResponse "recovery_not_ready"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/recovery/complete [post]
func (h *PublicHandler) CompleteRecovery(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req RecoveryCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	errs := map[string]string{}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		errs["challenge"] = "is required"
	}
	if req.NewPassword == "" {
		errs["new_password"] = "is required"
	}
	invalid := func() {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
	}
	if len(errs) > 0 {
		invalid()
		return
	}
	if h.RedisClient == nil || h.PostgresDB == nil || h.Passwords == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	ch, ok := h.challenge(c, ctx, req.Challenge)
	if !ok {
		return
	}
	userHK := ch.UserHK.String()

	result, err := h.Passwords.CompleteRecovery(ctx, userHK, ch.ContactID, req.NewPassword, audit.FromRequest(c))
	var notReady *userauth.RecoveryNotReadyError
	switch {
	case errors.Is(err, userauth.ErrPasswordTooShort):
		errs["new_password"] = "must be at least " + strconv.Itoa(h.Passwords.MinPasswordLength()) + " characters"
		invalid()
		return
	case errors.Is(err, userauth.ErrPasswordTooLong):
		errs["new_password"] = "must be valid text of at most " + strconv.Itoa(userauth.MaxPasswordBytes) + " bytes"
		invalid()
		return
	case errors.Is(err, userauth.ErrPasswordTooSimple):
		errs["new_password"] = "must have at least five different characters"
		invalid()
		return
	case errors.Is(err, userauth.ErrNoRecovery), errors.Is(err, userauth.ErrNoPassword):
		helpers.SendError(c, http.StatusNotFound, codeRecoveryNotFound, i18n.M("RECOVERY_NOT_FOUND"), nil)
		return
	case errors.Is(err, userauth.ErrRecoveryContact):
		helpers.SendError(c, http.StatusUnauthorized, codeRecoveryContact, i18n.M("RECOVERY_CONTACT_INVALID"), nil)
		return
	case errors.As(err, &notReady):
		info := gin.H{}
		if !notReady.ReadyAt.IsZero() {
			info["ready_at"] = notReady.ReadyAt
			c.Header("Retry-After", strconv.Itoa(max(1, int(time.Until(notReady.ReadyAt).Seconds())+1)))
		}
		helpers.SendError(c, http.StatusConflict, codeRecoveryNotReady, i18n.M("RECOVERY_NOT_READY"), info)
		return
	case errors.Is(err, rowseal.ErrUnverified):
		// The password row was changed outside the backend (#139): the
		// recovery would seal over it, so it waits for an administrator.
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_CANNOT_SIGN_IN"), nil)
		return
	case err != nil:
		log.Printf("Failed to complete the recovery of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECOVER_ACCOUNT"), nil)
		return
	}

	// The password is replaced. The challenge is spent, and whoever held a
	// session of the account signed in with the old password.
	if err := h.RedisClient.Client.Del(ctx, ch.Key).Err(); err != nil {
		log.Printf("Failed to remove a second-step challenge after a recovery: %v", err)
	}
	if ender, ok := h.Sessions.(sessionEnder); ok {
		revoke := auth.SessionEvent(c, audit.ActionUserSessionRevoke, userHK, "")
		if _, err := ender.RevokeOthers(ctx, userHK, uuid.Nil.String(), revoke); err != nil {
			// The password is already the new one, so nobody signs in
			// with the old one; the sessions that remain are reported.
			log.Printf("Failed to end the sessions of a user after a recovery: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_RECOVER_ACCOUNT"), nil)
			return
		}
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("RECOVERY_COMPLETED"), RecoveryCompleteInfo{
		RecoveryCodes: result.RecoveryCodes, HoldUntil: result.HoldUntil,
	})
}
