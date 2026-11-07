package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// otpNamespace keeps admin sign-in codes, sessions, and limits apart from
// those of user sign-in.
const otpNamespace = "admin"

// signInCodeSent is the answer to every valid sign-in request, so that it does
// not reveal which addresses belong to administrators.
const signInCodeSent = "If this address belongs to an administrator, a sign-in code was sent to it"

type adminSignInRequest struct {
	Email string `json:"email"`
}

type adminVerifyRequest struct {
	OTP  string `json:"otp"`
	TOTP string `json:"totp"`
}

// signInAvailable reports whether admin sign-in is configured: admin signing
// keys, the TOTP key, and email delivery.
func (h *AdminHandler) signInAvailable() bool {
	return h.Accounts != nil && h.OTP != nil && h.OTPSender != nil &&
		auth.KeysLoaded(auth.RoleAdmin) && h.OTPSender.Supports(notify.ContactEmail)
}

// RequireActiveAdmin runs after auth.JWTAdminVerification and checks the
// stored record of the token's administrator on every request. A token stays
// valid only while its administrator keeps the admin role, the approved
// status, and an enrolled authenticator, so demoting or disabling an
// administrator takes effect at once instead of when its token expires.
func (h *AdminHandler) RequireActiveAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := auth.GetClaims(c)
		if !ok || h.Accounts == nil {
			// Without admin sign-in, no admin token is legitimate.
			helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, "Unauthorized - Invalid token", nil)
			c.Abort()
			return
		}
		active, err := h.Accounts.IsActive(c.Request.Context(), claims.UUID)
		if err != nil {
			log.Printf("Failed to check administrator %s: %v", claims.UUID, err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to check the administrator", nil)
			c.Abort()
			return
		}
		if !active {
			log.Printf("Rejected a token of administrator %s, who is no longer active", claims.UUID)
			helpers.SendError(c, http.StatusUnauthorized, "admin_inactive", "Unauthorized - Administrator is not active", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequestSignIn starts an admin sign-in: it emails a one-time code to the
// address when it belongs to an active administrator. The response is the
// same either way.
//
// @Summary Request an admin sign-in code
// @Description Sends a one-time code to the administrator's email address. The answer is the same whether or not the address belongs to an administrator.
// @Tags admin sign-in
// @Accept json
// @Produce json
// @Param body body adminSignInRequest true "The administrator's email address"
// @Success 200 {object} models.APIResponse{info=models.SignInSessionInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/auth [post]
func (h *AdminHandler) RequestSignIn(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if !h.signInAvailable() {
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Admin sign-in is not available", nil)
		return
	}
	var req adminSignInRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: Email is required", nil)
		return
	}
	email, err := adminauth.NormalizeEmail(req.Email)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid email address", nil)
		return
	}

	// From here on, an administrator's address and an unknown one must be
	// told apart neither by the answer nor by its duration (#190): both
	// issue a code under the same limits, and every answer waits until the
	// same minimum time has passed. Only the email of an administrator is
	// sent, which takes longer, mostly for the confirm of the broker. The
	// wait comes before the answer is written, since a written answer may
	// already be on its way.
	start := time.Now()
	minimum := time.Duration(config.Config.Admin.SignInMinResponseMS) * time.Millisecond
	respond := func(status int, message string, info interface{}) {
		padResponse(ctx, start, minimum)
		helpers.SendResponse(c, status, message, info)
	}

	admin, err := h.Accounts.FindActive(ctx, email)
	known := err == nil
	if err != nil && !errors.Is(err, adminauth.ErrNotFound) {
		log.Printf("Failed to look up an administrator for sign-in: %v", err)
		respond(http.StatusInternalServerError, "Failed to start the sign-in", nil)
		return
	}
	adminID := ""
	if known {
		adminID, email = admin.ID, admin.Email
	}

	// As for users, the per-contact limits count by the client that asks,
	// so that a stranger cannot lock an administrator out (#187). An
	// unknown address gets a code too, which is never sent and expires, so
	// that it reaches the limits after as many requests as an
	// administrator's address, and its session leads nowhere.
	issued, err := h.OTP.Issue(ctx, email, clientip.Key(c.ClientIP(), config.Config.RateLimits.IPv6PrefixLength))
	switch {
	case errors.Is(err, otp.ErrTooManyRequests):
		h.noteSignIn(ctx, c, audit.ActionAdminSignInCode, adminID, audit.Denied, "rate_limited")
		respond(http.StatusTooManyRequests, "Too many sign-in codes requested. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.noteSignIn(ctx, c, audit.ActionAdminSignInCode, adminID, audit.Denied, "rate_limited")
		respond(http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case err != nil:
		log.Printf("Failed to issue an admin sign-in code: %v", err)
		respond(http.StatusInternalServerError, "Failed to start the sign-in", nil)
		return
	}
	if !known {
		h.noteSignIn(ctx, c, audit.ActionAdminSignInCode, "", audit.Failure, "unknown_account")
		respond(http.StatusOK, signInCodeSent, gin.H{"uuid": issued.SessionID})
		return
	}

	err = h.OTPSender.SendOTP(ctx, notify.OTPMessage{
		ContactType:   notify.ContactEmail,
		Contact:       admin.Email,
		MaskedContact: adminauth.MaskEmail(admin.Email),
		Code:          issued.Code,
	})
	if err != nil {
		log.Printf("Failed to send the admin sign-in code to administrator %s: %v", admin.ID, err)
		respond(http.StatusInternalServerError, "Failed to start the sign-in", nil)
		return
	}
	h.noteSignIn(ctx, c, audit.ActionAdminSignInCode, admin.ID, audit.Success, "")
	respond(http.StatusOK, signInCodeSent, gin.H{"uuid": issued.SessionID})
}

// VerifySignIn completes an admin sign-in with the emailed code and a code
// from the administrator's authenticator app, and returns an admin access
// token. Every failure gets the same answer, so it does not reveal which
// factor was wrong.
//
// @Summary Verify an admin sign-in
// @Description Checks the emailed code and the authenticator (TOTP) code, and returns an admin access token.
// @Tags admin sign-in
// @Accept json
// @Produce json
// @Param uuid path string true "The sign-in session from the code request" format(uuid)
// @Param body body adminVerifyRequest true "The emailed code and the authenticator code"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/auth/{uuid} [post]
func (h *AdminHandler) VerifySignIn(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	if !h.signInAvailable() {
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Admin sign-in is not available", nil)
		return
	}
	sessionID := c.Param("uuid")
	if _, err := uuid.Parse(sessionID); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format", nil)
		return
	}
	var req adminVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.OTP == "" || req.TOTP == "" {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid request data: OTP and TOTP are required", nil)
		return
	}
	// An authenticator code typed in Persian digits is the same code (#197);
	// the emailed code is normalized by the OTP store.
	req.TOTP, _ = contact.Code(req.TOTP, totp.Digits)

	// The emailed code is checked first and ends the session, so every
	// guess of the authenticator code needs a new emailed code.
	email, err := h.OTP.Verify(ctx, sessionID, req.OTP)
	switch {
	case errors.Is(err, otp.ErrTooManyAttempts):
		h.noteSignIn(ctx, c, audit.ActionAdminSignIn, "", audit.Denied, "rate_limited")
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrInvalidCode):
		h.noteSignIn(ctx, c, audit.ActionAdminSignIn, "", audit.Failure, "email_code_invalid")
		helpers.SendError(c, http.StatusUnauthorized, "code_invalid", "Invalid or expired code", nil)
		return
	case err != nil:
		log.Printf("Failed to verify an admin sign-in code: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to verify the sign-in", nil)
		return
	}

	admin, err := h.Accounts.FindActive(ctx, email)
	if err == nil {
		err = h.Accounts.VerifyTOTP(ctx, admin.ID, req.TOTP)
	}
	switch {
	case errors.Is(err, adminauth.ErrNotFound), errors.Is(err, adminauth.ErrInvalidCode):
		log.Printf("Rejected an admin sign-in for %s: %v", adminauth.MaskEmail(email), err)
		if errors.Is(err, adminauth.ErrNotFound) {
			h.noteSignIn(ctx, c, audit.ActionAdminSignIn, "", audit.Failure, "account_unavailable")
		} else {
			h.noteSignIn(ctx, c, audit.ActionAdminSignIn, admin.ID, audit.Failure, "authenticator_code_invalid")
		}
		helpers.SendError(c, http.StatusUnauthorized, "code_invalid", "Invalid or expired code", nil)
		return
	case err != nil:
		log.Printf("Failed to verify an admin sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to verify the sign-in", nil)
		return
	}

	// The code proved control of the address. A failure to record that does
	// not make the sign-in less valid.
	if err := h.Accounts.ConfirmEmail(ctx, admin.ID); err != nil {
		log.Printf("Failed to confirm the email address of administrator %s: %v", admin.ID, err)
	}

	// Admin tokens have no refresh token: an administrator signs in again
	// with both factors when the access token expires.
	accessToken, err := auth.GenerateAccessToken(admin.ID, auth.RoleAdmin, "")
	if err != nil {
		log.Printf("Failed to issue an admin token: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to generate access token", nil)
		return
	}
	// An administrator signs in only with a record of it.
	if err := h.auditSignIn(ctx, c, audit.ActionAdminSignIn, admin.ID, audit.Success, ""); err != nil {
		log.Printf("Refused the sign-in of administrator %s, which could not be recorded: %v", admin.ID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to verify the sign-in", nil)
		return
	}
	log.Printf("Administrator %s signed in", admin.ID)
	helpers.SendResponse(c, http.StatusOK, "Signed in successfully", gin.H{"access_token": accessToken})
}

// padResponse waits until minimum has passed since start, so that every
// answer it pads takes at least as long. It returns early when ctx ends.
func padResponse(ctx context.Context, start time.Time, minimum time.Duration) {
	wait := time.Until(start.Add(minimum))
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// auditSignIn records an event of the admin sign-in. It is written on its
// own: an attempt changes nothing that it could share a transaction with.
// An empty adminID is an anonymous actor, such as an unknown address.
func (h *AdminHandler) auditSignIn(ctx context.Context, c *gin.Context, action, adminID, outcome, reason string) error {
	e := audit.FromRequest(c)
	e.Action, e.Outcome, e.Reason = action, outcome, reason
	e.ActorType = audit.ActorAnonymous
	if adminID != "" {
		e.ActorType, e.ActorID = audit.ActorAdmin, adminID
	}
	return audit.Record(ctx, h.PostgresDB.DB, e)
}

// noteSignIn records an event that must not block the answer: an attempt
// that failed or was refused. A failure to record it is logged, so that an
// unavailable audit log cannot be used to lock administrators out.
func (h *AdminHandler) noteSignIn(ctx context.Context, c *gin.Context, action, adminID, outcome, reason string) {
	if err := h.auditSignIn(ctx, c, action, adminID, outcome, reason); err != nil {
		log.Printf("Failed to record the audit event %s (%s): %v", action, outcome, err)
	}
}

// Logout revokes the admin access token of the request, and records it.
// The record is best effort: a sign-out is never refused.
func (h *AdminHandler) Logout(tb *auth.TokenBlacklist) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth.LogoutHandler(c, tb, nil)
		claims, ok := auth.GetClaims(c)
		if !ok || c.Writer.Status() != http.StatusOK {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		defer cancel()
		h.noteSignIn(ctx, c, audit.ActionAdminLogout, claims.UUID, audit.Success, "")
	}
}
