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
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/agreements"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/signin"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// Sign-up with a record of what was accepted (#316). A correct code for a
// contact without an account creates one only with an acceptance of the
// current version of every required document. Without it the answer is 428
// with the documents and a challenge, and the acceptance, sent with the
// challenge, creates the account. Only the holder of a correct code sees
// the difference, so it does not say who has an account.

const (
	// signUpTTL is how long a sign-up challenge can be used.
	signUpTTL = 10 * time.Minute
	// signUpKeyPrefix is the Redis key prefix of the challenges.
	signUpKeyPrefix = "sign_up:"

	codeSignUpTerms   = "signup_terms_required"
	codeSignUpCountry = "signup_country_unsupported"
)

// signUpInfo is what a new account needs from its request.
type signUpInfo = signin.SignUp

// signUpOf returns the sign-up information of a request.
func (h *PublicHandler) signUpOf(c *gin.Context, accept map[string]string) signUpInfo {
	info := signUpInfo{Accept: accept}
	if h.GeoIP != nil {
		info.Country = h.GeoIP.Resolve(c).Country
	}
	return info
}

// SignUpTermsInfo is the info of a 428 signup_terms_required.
type SignUpTermsInfo struct {
	// Challenge creates the account with POST /public/auth/sign-up.
	Challenge string `json:"challenge"`
	// ExpiresIn is how long the challenge can be used, in seconds.
	ExpiresIn int `json:"expires_in" example:"600"`
	// Documents are the documents to show and accept, in their current
	// versions.
	Documents []agreements.Document `json:"documents"`
}

// SignUpRequest creates an account with the acceptance of its holder.
type SignUpRequest struct {
	Challenge string `json:"challenge"`
	// Accept are the versions that the holder accepted, by kind.
	Accept map[string]string `json:"accept" example:"terms:2026-11,privacy:2026-11"`
}

func signUpKey(challenge string) string {
	sum := sha256.Sum256([]byte(challenge))
	return signUpKeyPrefix + hex.EncodeToString(sum[:])
}

// askSignUpTerms answers a correct code of a contact without an account
// with the documents to accept, and a challenge that stands for the code.
func (h *PublicHandler) askSignUpTerms(c *gin.Context, ctx context.Context, contactType, contact string, attempt audit.Event) {
	if h.Redis == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails since Go 1.24
	challenge := base64.RawURLEncoding.EncodeToString(raw)
	if err := h.Redis.Set(ctx, signUpKey(challenge), contactType+"|"+contact, signUpTTL).Err(); err != nil {
		log.Printf("Failed to store a sign-up challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_USER"), nil)
		return
	}
	h.noteSignIn(ctx, attempt, audit.Denied, codeSignUpTerms)
	helpers.SendError(c, http.StatusPreconditionRequired, codeSignUpTerms, i18n.M("SIGN_UP_TERMS_REQUIRED"), SignUpTermsInfo{
		Challenge: challenge, ExpiresIn: int(signUpTTL.Seconds()), Documents: agreements.Required(h.settings().Legal),
	})
}

// @Summary Create an account with the accepted documents
// @Description A correct code for a contact without an account answers 428 signup_terms_required, with the legal documents in their current versions and a challenge. This route creates the account once its holder accepted them: accept has the version of each document by kind. The acceptance is stored with its time, address, user agent, and country. The answer is that of a sign-in. The challenge works for ten minutes, until an account is created with it; an acceptance that misses a document or names another version answers 428 signup_terms_required again, with the same challenge. 403 signup_country_unsupported: the country of the request is in legal.signup.blocked_countries.
// @Tags sign-in
// @Accept json
// @Produce json
// @Param body body SignUpRequest true "The challenge and the accepted versions"
// @Success 200 {object} models.APIResponse{info=models.AccessTokenInfo} "The access token. The refresh token is set as an HttpOnly cookie."
// @Header 200 {string} Set-Cookie "The refresh token, HttpOnly"
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse "challenge_invalid"
// @Failure 403 {object} models.APIResponse "signup_country_unsupported, or the account cannot sign in"
// @Failure 428 {object} models.APIResponse{info=SignUpTermsInfo} "signup_terms_required"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/public/auth/sign-up [post]
func (h *PublicHandler) SignUp(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var req SignUpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if req.Challenge == "" || len(req.Challenge) > 128 {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"),
			gin.H{"errors": map[string]string{"challenge": "is required"}})
		return
	}
	if h.Redis == nil || h.Accounts == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("SIGN_IN_IS_NOT_AVAILABLE"), nil)
		return
	}
	key := signUpKey(req.Challenge)
	value, err := h.Redis.Get(ctx, key).Result()
	contactType, contact, found := strings.Cut(value, "|")
	if errors.Is(err, redis.Nil) || (err == nil && (!found || !signin.SignInContactTypes[contactType] || contact == "")) {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	}
	if err != nil {
		log.Printf("Failed to read a sign-up challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_USER"), nil)
		return
	}
	attempt := audit.FromRequest(c)
	attempt.Action, attempt.ActorType = audit.ActionUserSignIn, audit.ActorAnonymous
	attempt.TargetType, attempt.TargetID = audit.TargetSignIn, c.GetHeader("X-Request-ID")

	if !agreements.Covers(h.settings().Legal, req.Accept) {
		// The challenge stays: the client shows the documents again.
		h.noteSignIn(ctx, attempt, audit.Denied, codeSignUpTerms)
		ttl, _ := h.Redis.TTL(ctx, key).Result()
		helpers.SendError(c, http.StatusPreconditionRequired, codeSignUpTerms, i18n.M("SIGN_UP_TERMS_REQUIRED"), SignUpTermsInfo{
			Challenge: req.Challenge, ExpiresIn: max(0, int(ttl.Seconds())), Documents: agreements.Required(h.settings().Legal),
		})
		return
	}
	// The challenge creates one account: of two requests with it, one
	// takes it.
	taken, err := h.Redis.Del(ctx, key).Result()
	if err != nil {
		log.Printf("Failed to take a sign-up challenge: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CREATE_USER"), nil)
		return
	}
	if taken == 0 {
		helpers.SendError(c, http.StatusUnauthorized, "challenge_invalid", i18n.M("CHALLENGE_INVALID_OR_EXPIRED"), nil)
		return
	}
	h.finishSignIn(c, ctx, contactType, contact, h.signUpOf(c, req.Accept), attempt)
}
