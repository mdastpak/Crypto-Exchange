package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Profile is the response of GET and PATCH /user/profile. The contacts are
// masked: the user proved control of them at sign-in, and a leaked response
// should not reveal them.
type Profile struct {
	UUID        string            `json:"uuid"`
	Status      string            `json:"status"`
	Contact     map[string]string `json:"contact"`
	FirstName   *string           `json:"first_name"`
	LastName    *string           `json:"last_name"`
	DisplayName *string           `json:"display_name"`
	KYCStatus   string            `json:"kyc_status"`
	KYCTier     string            `json:"kyc_tier"`
	CreatedAt   *time.Time        `json:"created_at"`
}

var (
	errProfileNotFound    = errors.New("profile not found")
	errAccountUnavailable = errors.New("account is not available")
)

// queryRower is implemented by *sql.DB and *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// profileQuery reads a profile. users.created_at is a timestamp without time
// zone, written with NOW() in the database's time zone, so it is converted
// to an instant in that same time zone.
// The contacts come as one JSON object of the primary contact of each type.
const profileQuery = `SELECT hk, status, role,
	COALESCE((SELECT jsonb_object_agg(c.type, c.value) FROM user_contacts c WHERE c.user_hk = users.hk AND c.is_primary AND c.removed_at IS NULL), '{}'),
	first_name, last_name, display_name, kyc_status, kyc_tier,
	created_at AT TIME ZONE current_setting('TimeZone')
	FROM users WHERE hk = $1 AND deleted_at IS NULL`

// loadProfile reads the profile of a user. It returns errProfileNotFound for
// a missing or deleted user, and errAccountUnavailable for an account that
// cannot sign in or is not a user account, so that a token issued before
// such a change no longer reaches the profile.
func loadProfile(ctx context.Context, db queryRower, userID uuid.UUID, lock bool) (Profile, error) {
	query := profileQuery
	if lock {
		query += " FOR UPDATE"
	}
	var p Profile
	var role string
	var contact []byte
	err := db.QueryRowContext(ctx, query, userID).Scan(&p.UUID, &p.Status, &role, &contact,
		&p.FirstName, &p.LastName, &p.DisplayName, &p.KYCStatus, &p.KYCTier, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, errProfileNotFound
	}
	if err != nil {
		return p, err
	}
	if !account.UserStatus(p.Status).CanSignIn() || role != auth.RoleUser {
		return p, errAccountUnavailable
	}

	var contacts map[string]string
	if err := json.Unmarshal(contact, &contacts); err != nil {
		return p, fmt.Errorf("decoding the contacts: %w", err)
	}
	p.Contact = make(map[string]string)
	for _, contactType := range []string{"email", "mobile"} {
		if value, ok := contacts[contactType]; ok {
			p.Contact[contactType] = maskContact(contactType, value)
		}
	}
	if p.CreatedAt != nil {
		created := p.CreatedAt.UTC()
		p.CreatedAt = &created
	}
	return p, nil
}

// maskContact hides most of a contact, like the sign-in logs do: the first
// three characters stay visible, with the domain of an email address or the
// last three digits of a mobile number.
func maskContact(contactType, value string) string {
	const hidden = "*****"
	if contactType == "email" {
		at := strings.LastIndex(value, "@")
		if at <= 0 {
			return hidden
		}
		return value[:min(3, at)] + hidden + value[at:]
	}
	if len(value) <= 6 {
		return hidden
	}
	return value[:3] + hidden + value[len(value)-3:]
}

// callerID returns the user that the access token names. There is no user ID
// in the profile routes, so a user can only read and change their own
// profile.
func callerID(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := auth.GetClaims(c)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(claims.UUID)
	return id, err == nil
}

// sendProfileError answers a failed profile read.
func sendProfileError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errProfileNotFound):
		helpers.SendResponse(c, http.StatusNotFound, i18n.M("PROFILE_NOT_FOUND"), nil)
	case errors.Is(err, errAccountUnavailable):
		helpers.SendError(c, http.StatusForbidden, helpers.CodeAccountUnavailable, i18n.M("ACCOUNT_NOT_AVAILABLE_CONTACT_SUPPORT"), nil)
	default:
		log.Printf("Failed to load a profile: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LOAD_PROFILE"), nil)
	}
}

// GetProfile returns the profile of the signed-in user.
//
// @Summary Get the signed-in user's profile
// @Tags user
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=Profile}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/profile [get]
func (h *UserHandler) GetProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	userID, ok := callerID(c)
	if !ok {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return
	}
	profile, err := loadProfile(ctx, h.PostgresDB.DB, userID, false)
	if err != nil {
		sendProfileError(c, err)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PROFILE_RETRIEVED_SUCCESSFULLY"), profile)
}

// UpdateProfile changes the names in the profile of the signed-in user and
// returns the updated profile. Only the fields in the request change.
//
// @Summary Change the names in the profile
// @Tags user
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body profileUpdate true "The names to change"
// @Success 200 {object} models.APIResponse{info=Profile}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/profile [patch]
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	userID, ok := callerID(c)
	if !ok {
		helpers.SendError(c, http.StatusUnauthorized, helpers.CodeTokenInvalid, i18n.M("UNAUTHORIZED_INVALID_TOKEN"), nil)
		return
	}
	update, errs, err := decodeProfileUpdate(c)
	if err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_REQUEST_BODY"), nil)
		return
	}
	if errs == nil {
		errs = update.validate()
	}
	if errs != nil {
		helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
		return
	}

	// The row is locked from the read to the write, so that the account
	// checks hold for the write, and concurrent updates of different
	// fields keep each other's changes.
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("Error starting a profile update: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_UPDATE_PROFILE"), nil)
		return
	}
	defer tx.Rollback()

	profile, err := loadProfile(ctx, tx, userID, true)
	if err != nil {
		sendProfileError(c, err)
		return
	}
	before := profile
	update.apply(&profile)
	_, err = tx.ExecContext(ctx,
		"UPDATE users SET first_name = $1, last_name = $2, display_name = $3, updated_at = NOW() WHERE hk = $4",
		profile.FirstName, profile.LastName, profile.DisplayName, userID,
	)
	if changes := nameChanges(before, profile); err == nil && changes != nil {
		// The change commits only with its event (#195).
		e := audit.FromRequest(c)
		e.ActorType, e.ActorID = audit.ActorUser, userID.String()
		e.Action, e.Outcome = audit.ActionUserProfileUpdate, audit.Success
		e.TargetType, e.TargetID = audit.TargetUser, userID.String()
		e.Changes = changes
		err = audit.Record(ctx, tx, e)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("Error updating a profile: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_UPDATE_PROFILE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("PROFILE_UPDATED_SUCCESSFULLY"), profile)
}
