package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/adminauth"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"

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
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Invalid token", nil)
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
			helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized - Administrator is not active", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequestSignIn starts an admin sign-in: it emails a one-time code to the
// address when it belongs to an active administrator. The response is the
// same either way.
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

	admin, err := h.Accounts.FindActive(ctx, email)
	if errors.Is(err, adminauth.ErrNotFound) {
		// A session ID that leads nowhere keeps the answer identical.
		helpers.SendResponse(c, http.StatusOK, signInCodeSent, gin.H{"uuid": uuid.NewString()})
		return
	}
	if err != nil {
		log.Printf("Failed to look up an administrator for sign-in: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to start the sign-in", nil)
		return
	}

	issued, err := h.OTP.Issue(ctx, admin.Email)
	switch {
	case errors.Is(err, otp.ErrTooManyRequests):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many sign-in codes requested. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrTooManyAttempts):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case err != nil:
		log.Printf("Failed to issue an admin sign-in code for administrator %s: %v", admin.ID, err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to start the sign-in", nil)
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
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to start the sign-in", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, signInCodeSent, gin.H{"uuid": issued.SessionID})
}

// VerifySignIn completes an admin sign-in with the emailed code and a code
// from the administrator's authenticator app, and returns an admin access
// token. Every failure gets the same answer, so it does not reveal which
// factor was wrong.
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

	// The emailed code is checked first and ends the session, so every
	// guess of the authenticator code needs a new emailed code.
	email, err := h.OTP.Verify(ctx, sessionID, req.OTP)
	switch {
	case errors.Is(err, otp.ErrTooManyAttempts):
		helpers.SendResponse(c, http.StatusTooManyRequests, "Too many failed attempts. Please try again later.", nil)
		return
	case errors.Is(err, otp.ErrInvalidCode):
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid or expired code", nil)
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
		helpers.SendResponse(c, http.StatusUnauthorized, "Invalid or expired code", nil)
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
	log.Printf("Administrator %s signed in", admin.ID)
	helpers.SendResponse(c, http.StatusOK, "Signed in successfully", gin.H{"access_token": accessToken})
}
