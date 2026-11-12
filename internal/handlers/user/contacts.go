package user

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

// Contact management (#199). A user holds at most one email address and one
// mobile number, and signs in with either. Every change needs two proofs:
// a code sent to a verified contact the user already has, which shows that
// the change is the account owner's and not only a stolen session's, and,
// to add or replace a contact, a code sent to the new contact, which shows
// that the user controls it.
//
// A change is started by one request and confirmed by another, which may
// reach another instance. Its state is a Redis hash, which every instance
// shares, and its codes are sessions of the OTP store in their own
// namespace, so that a code of a change can never sign in and a sign-in
// code can never confirm a change. The confirmation applies the change in
// one transaction that first locks the user's row, so that concurrent
// changes of one user, on any instance, are applied one after the other
// and see each other's result.

// ContactOTPNamespace keeps the codes of contact changes apart from sign-in.
const ContactOTPNamespace = "contact"

// Contact change actions.
const (
	contactAdd     = "add"
	contactReplace = "replace"
	contactRemove  = "remove"
)

// Stable error codes of the contact routes.
const (
	codeContactExists   = "contact_exists"
	codeContactNotFound = "contact_not_found"
	codeLastContact     = "last_contact"
	codeSameContact     = "same_contact"
	codeContactInUse    = "contact_in_use"
	codeChangeNotFound  = "change_not_found"
	codeCodeInvalid     = "code_invalid"
	codeRateLimited     = "rate_limited"
	codeChannelMissing  = "channel_unavailable"
	codeContactTooNew   = "contact_too_new"
	// The mobile ownership check (#253).
	codeMobileOwnerMismatch = "mobile_owner_mismatch"
	codeMobileRequired      = "mobile_required"
	codeIdentityUnavailable = "identity_unavailable"
	codeIdentityChanged     = "identity_changed"
)

// changeKeyPrefix is the Redis key prefix of pending changes.
const changeKeyPrefix = "contact_change:"

// Contact is a contact of the signed-in user, as GET /user/contacts shows
// it. The value is masked, as in the profile.
type Contact struct {
	Type       string    `json:"type" example:"mobile"`
	Value      string    `json:"value" example:"091*****789"`
	VerifiedAt time.Time `json:"verified_at"`
}

// ContactList is the info of GET /user/contacts.
type ContactList struct {
	Items []Contact `json:"items"`
}

// ContactChangeRequest starts a change.
type ContactChangeRequest struct {
	// Action is add, replace, or remove.
	Action string `json:"action" example:"add" enums:"add,replace,remove"`
	// Type is the contact to remove. For add and replace, it is taken from
	// Value and may be left out.
	Type string `json:"type,omitempty" example:"mobile" enums:"email,mobile"`
	// Value is the new contact, for add and replace.
	Value string `json:"value,omitempty" example:"09123456789"`
	// VerifyWith is the verified contact that receives the code that proves
	// the change is the account owner's. By default, the contact that is
	// replaced or removed, or for add, the other contact.
	VerifyWith string `json:"verify_with,omitempty" example:"email" enums:"email,mobile"`
}

// ContactChangeStarted is the info of a started change. The codes are sent
// to the masked contacts named here.
type ContactChangeStarted struct {
	ChangeID string `json:"change_id" format:"uuid"`
	// AccountCodeSentTo is the verified contact that received the code of
	// account_code.
	AccountCodeSentTo string `json:"account_code_sent_to" example:"ali*****@example.com"`
	// NewCodeSentTo is the new contact that received the code of new_code.
	// It is left out for a removal.
	NewCodeSentTo string `json:"new_code_sent_to,omitempty" example:"091*****789"`
	// ExpiresIn is the number of seconds the codes are valid.
	ExpiresIn int `json:"expires_in" example:"60"`
}

// ContactChangeConfirmation confirms a change with its codes. A code that
// was accepted by an earlier confirmation of the same change may be left
// out.
type ContactChangeConfirmation struct {
	AccountCode string `json:"account_code,omitempty" example:"123456"`
	NewCode     string `json:"new_code,omitempty" example:"654321"`
}

// pendingChange is a started change, as its Redis hash holds it.
type pendingChange struct {
	UserID, Action, Type, Value string
	// AccountContact and AccountSession are the verified contact that
	// received the account code, and the OTP session of that code.
	AccountContact, AccountSession string
	NewSession                     string
	AccountProven, NewProven       bool
}

// ListContacts returns the contacts of the signed-in user.
//
// @Summary List my contacts
// @Description The verified email address and mobile number of the signed-in user, masked. Either one signs in.
// @Tags user contacts
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=ContactList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/contacts [get]
func (h *UserHandler) ListContacts(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	contacts, _, err := verifiedContacts(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to list the contacts of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_LIST_CONTACTS"), nil)
		return
	}
	list := ContactList{Items: make([]Contact, 0, len(contacts))}
	for _, ct := range contacts {
		list.Items = append(list.Items, Contact{Type: ct.typ, Value: maskContact(ct.typ, ct.value), VerifiedAt: ct.verifiedAt.UTC()})
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTACTS_RETRIEVED_SUCCESSFULLY"), list)
}

// storedContact is a verified contact as the database holds it.
type storedContact struct {
	typ, value string
	verifiedAt time.Time
}

// verifiedContacts returns the verified email and mobile contacts of a user,
// email first, and the time of the database when it read them. The rules
// that depend on the age of a contact compare with that time, never with
// the clock of the instance, so that every instance decides the same way.
// It is the zero time when the user has no verified contact.
func verifiedContacts(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}, userHK string) ([]storedContact, time.Time, error) {
	rows, err := q.QueryContext(ctx, `SELECT type::text, value, verified_at, NOW() FROM user_contacts c
		WHERE user_hk = $1 AND is_primary AND verified_at IS NOT NULL AND type IN ('email', 'mobile') AND c.removed_at IS NULL
		ORDER BY type`, userHK)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []storedContact
	var now time.Time
	for rows.Next() {
		var ct storedContact
		if err := rows.Scan(&ct.typ, &ct.value, &ct.verifiedAt, &now); err != nil {
			return nil, time.Time{}, err
		}
		out = append(out, ct)
	}
	return out, now, rows.Err()
}

func findContact(contacts []storedContact, typ string) (storedContact, bool) {
	for _, ct := range contacts {
		if ct.typ == typ {
			return ct, true
		}
	}
	return storedContact{}, false
}

// contactChange is the validated plan of a change against the current
// contacts.
type contactChange struct {
	action, typ, value string
	account            storedContact
}

// errChange is a refusal of a change, with its status, code, and message,
// and the info of the answer, if any.
type errChange struct {
	status  int
	code    string
	message i18n.Message
	info    gin.H
}

func (e *errChange) Error() string { return e.message.String() }

// planChange checks a change against the current contacts of the user, as
// the database read them at now. It runs when the change starts, and again
// under the row lock when it is applied, since the contacts may have
// changed in between.
func planChange(cooldown time.Duration, req ContactChangeRequest, contacts []storedContact, now time.Time) (contactChange, error) {
	ch := contactChange{action: req.Action, typ: req.Type, value: req.Value}
	current, has := findContact(contacts, ch.typ)
	switch ch.action {
	case contactAdd:
		if has {
			return ch, &errChange{status: http.StatusConflict, code: codeContactExists, message: i18n.M("CONTACT_TYPE_EXISTS")}
		}
	case contactReplace:
		if !has {
			return ch, &errChange{status: http.StatusConflict, code: codeContactNotFound, message: i18n.M("NO_CONTACT_TO_REPLACE")}
		}
		if strings.EqualFold(current.value, ch.value) {
			return ch, &errChange{status: http.StatusConflict, code: codeSameContact, message: i18n.M("CONTACT_UNCHANGED")}
		}
	case contactRemove:
		if !has {
			return ch, &errChange{status: http.StatusConflict, code: codeContactNotFound, message: i18n.M("NO_CONTACT_TO_REMOVE")}
		}
		if len(contacts) < 2 {
			return ch, &errChange{status: http.StatusConflict, code: codeLastContact, message: i18n.M("LAST_CONTACT_CANNOT_BE_REMOVED")}
		}
	}

	verifyWith := req.VerifyWith
	if verifyWith == "" {
		if ch.action != contactAdd {
			verifyWith = ch.typ
		} else if len(contacts) > 0 {
			verifyWith = contacts[0].typ
		}
	}
	account, ok := findContact(contacts, verifyWith)
	if !ok {
		return ch, &errChange{status: http.StatusConflict, code: codeContactNotFound, message: i18n.M("VERIFYING_CONTACT_NOT_YOURS")}
	}
	if err := checkCooldown(cooldown, ch, account, current, now); err != nil {
		return ch, err
	}
	ch.account = account
	return ch, nil
}

// checkCooldown refuses a replacement or removal of the contact target
// that is proven with account, another contact that was verified after
// target and less than contacts.new_contact_cooldown_hours before now
// (#250). Without it, someone with a stolen session who adds a contact of
// their own could use it at once to replace or remove the owner's contact,
// and take the account. The older contact can always change the newer one,
// so that the owner can undo an unwanted new contact at once, and a
// contact can always be changed with its own code. Adding is never
// refused: it takes nothing away.
func checkCooldown(cooldown time.Duration, ch contactChange, account, target storedContact, now time.Time) error {
	if ch.action == contactAdd || account.typ == ch.typ || !account.verifiedAt.After(target.verifiedAt) {
		return nil
	}
	if cooldown <= 0 {
		return nil
	}
	availableAt := account.verifiedAt.Add(cooldown)
	if !now.Before(availableAt) {
		return nil
	}
	return &errChange{
		status:  http.StatusForbidden,
		code:    codeContactTooNew,
		message: i18n.M("VERIFYING_CONTACT_TOO_RECENT"),
		info:    gin.H{"available_at": availableAt.UTC().Format(time.RFC3339)},
	}
}

// validateChangeRequest normalizes the request and names its invalid
// fields.
func validateChangeRequest(req *ContactChangeRequest) map[string]string {
	errs := map[string]string{}
	switch req.Action {
	case contactAdd, contactReplace:
		req.Value = contact.Normalize(req.Value)
		typ, _ := contact.Identify(req.Value)
		if typ == "" {
			errs["value"] = "must be an email address or an Iranian mobile number"
		} else if req.Type != "" && req.Type != typ {
			errs["type"] = "must match the type of value"
		}
		req.Type = typ
	case contactRemove:
		if req.Type != notify.ContactEmail && req.Type != notify.ContactMobile {
			errs["type"] = "must be email or mobile"
		}
		if req.Value != "" {
			errs["value"] = "must be left out for a removal"
		}
	default:
		errs["action"] = "must be add, replace, or remove"
	}
	if req.VerifyWith != "" && req.VerifyWith != notify.ContactEmail && req.VerifyWith != notify.ContactMobile {
		errs["verify_with"] = "must be email or mobile"
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func sendContactValidation(c *gin.Context, errs map[string]string) {
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
}

// contactsReady reports whether the contact routes have what they need.
func (h *UserHandler) contactsReady(c *gin.Context) bool {
	if h.ContactOTP == nil || h.OTPSender == nil || h.PostgresDB == nil || h.RedisClient == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, i18n.M("CONTACT_CHANGES_ARE_NOT_AVAILABLE"), nil)
		return false
	}
	return true
}

// StartContactChange starts adding, replacing, or removing a contact.
//
// @Summary Start a contact change
// @Description Sends a code to a verified contact of the account and, to add or replace a contact, a code to the new one. Confirm the change with both codes at POST /user/contacts/changes/{id}. A user holds at most one email address and one mobile number; the last verified contact cannot be removed.
// @Tags user contacts
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body ContactChangeRequest true "The change"
// @Success 200 {object} models.APIResponse{info=ContactChangeStarted}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "identity_unavailable: the mobile number cannot be checked against the national ID now"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/contacts/changes [post]
func (h *UserHandler) StartContactChange(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	if !h.contactsReady(c) {
		return
	}
	var req ContactChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}
	if errs := validateChangeRequest(&req); errs != nil {
		sendContactValidation(c, errs)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	contacts, now, err := verifiedContacts(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to read the contacts of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_START_THE_CHANGE"), nil)
		return
	}
	plan, err := planChange(cooldownOf(h.settings()), req, contacts, now)
	if sendRefusal(c, err) {
		return
	}
	// A user with a verified national ID keeps a mobile number (#253):
	// the removal is refused before any code is sent. The confirmation
	// checks it again.
	if plan.action == contactRemove && plan.typ == notify.ContactMobile {
		_, has, err := h.nationalID(ctx, h.PostgresDB.DB, userID.String())
		if sendRefusal(c, err) {
			return
		}
		if err != nil {
			log.Printf("Failed to read the national ID of a user: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_START_THE_CHANGE"), nil)
			return
		}
		if has {
			sendRefusal(c, errMobileRequired)
			return
		}
	}
	for _, typ := range []string{plan.account.typ, plan.newType()} {
		if typ != "" && !h.OTPSender.Supports(typ) {
			helpers.SendError(c, http.StatusConflict, codeChannelMissing, i18n.M("CODES_CANNOT_BE_SENT_BY", i18n.A("channel", i18n.Key("CHANNEL_"+strings.ToUpper(typ)))), nil)
			return
		}
	}

	// A contact that belongs to another user is not refused here, so that
	// the answer does not tell whether it is taken; the code proves control
	// of it first, and only then may the refusal say so.
	client := clientip.Key(c.ClientIP(), h.settings().RateLimits.IPv6PrefixLength)
	change := pendingChange{UserID: userID.String(), Action: plan.action, Type: plan.typ, Value: plan.value, AccountContact: plan.account.value}
	accountIssued, err := h.issue(ctx, plan.account.value, plan.account.typ, client)
	if sendIssueError(c, err) {
		return
	}
	change.AccountSession = accountIssued.SessionID
	started := ContactChangeStarted{AccountCodeSentTo: maskContact(plan.account.typ, plan.account.value), ExpiresIn: int(h.ContactOTP.TTL().Seconds())}
	if plan.action != contactRemove {
		newIssued, err := h.issue(ctx, plan.value, plan.typ, client)
		if sendIssueError(c, err) {
			return
		}
		change.NewSession = newIssued.SessionID
		started.NewCodeSentTo = maskContact(plan.typ, plan.value)
	} else {
		change.NewProven = true
	}

	started.ChangeID = uuid.NewString()
	if err := h.saveChange(ctx, started.ChangeID, change); err != nil {
		log.Printf("Failed to store a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_START_THE_CHANGE"), nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CHANGE_CODES_SENT"), started)
}

func (p contactChange) newType() string {
	if p.action == contactRemove {
		return ""
	}
	return p.typ
}

// issue issues a code for a contact and sends it.
func (h *UserHandler) issue(ctx context.Context, value, typ, client string) (otp.Issued, error) {
	issued, err := h.ContactOTP.Issue(ctx, value, client)
	if err != nil {
		return issued, err
	}
	_, masked := contact.Identify(value)
	return issued, h.OTPSender.SendOTP(ctx, notify.OTPMessage{ContactType: typ, Contact: value, MaskedContact: masked, Code: issued.Code})
}

// sendRefusal answers a refused change and reports whether it did.
func sendRefusal(c *gin.Context, err error) bool {
	var refusal *errChange
	if errors.As(err, &refusal) {
		var info interface{}
		if refusal.info != nil {
			info = refusal.info
		}
		helpers.SendError(c, refusal.status, refusal.code, refusal.message, info)
		return true
	}
	return false
}

// sendIssueError answers a code that could not be issued or sent, and
// reports whether it did.
func sendIssueError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, otp.ErrTooManyRequests), errors.Is(err, otp.ErrTooManyAttempts):
		helpers.SendError(c, http.StatusTooManyRequests, codeRateLimited, i18n.M("TOO_MANY_CODES_REQUESTED"), nil)
	default:
		log.Printf("Failed to send a contact change code: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_SEND_THE_CODE"), nil)
	}
	return true
}

func (h *UserHandler) changeKey(id string) string { return changeKeyPrefix + id }

// saveChange stores a started change for as long as its codes are valid.
func (h *UserHandler) saveChange(ctx context.Context, id string, ch pendingChange) error {
	key := h.changeKey(id)
	pipe := h.RedisClient.Client.TxPipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"user": ch.UserID, "action": ch.Action, "type": ch.Type, "value": ch.Value,
		"account_contact": ch.AccountContact, "account_session": ch.AccountSession,
		"new_session": ch.NewSession, "account_ok": flag(ch.AccountProven), "new_ok": flag(ch.NewProven),
	})
	pipe.Expire(ctx, key, h.ContactOTP.TTL())
	_, err := pipe.Exec(ctx)
	return err
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// loadChange reads a pending change of userID. A change of another user is
// reported as missing, so that its id tells nothing.
func (h *UserHandler) loadChange(ctx context.Context, id, userID string) (pendingChange, bool, error) {
	fields, err := h.RedisClient.Client.HGetAll(ctx, h.changeKey(id)).Result()
	if err != nil {
		return pendingChange{}, false, err
	}
	if len(fields) == 0 || fields["user"] != userID {
		return pendingChange{}, false, nil
	}
	return pendingChange{
		UserID: fields["user"], Action: fields["action"], Type: fields["type"], Value: fields["value"],
		AccountContact: fields["account_contact"], AccountSession: fields["account_session"],
		NewSession: fields["new_session"], AccountProven: fields["account_ok"] == "1", NewProven: fields["new_ok"] == "1",
	}, true, nil
}

// ConfirmContactChange confirms a change with its codes and applies it.
//
// @Summary Confirm a contact change
// @Description Applies the change once both codes are right. For a user with a verified national ID, a new mobile number must be registered to it (403 mobile_owner_mismatch), and the mobile number cannot be removed (409 mobile_required). A wrong code answers 401 code_invalid and names it in info.field; the codes already accepted stay accepted, so a retry needs only the others. Codes expire with the change.
// @Tags user contacts
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The change_id of the started change" format(uuid)
// @Param body body ContactChangeConfirmation true "The codes"
// @Success 200 {object} models.APIResponse{info=ContactList}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 429 {object} models.APIResponse
// @Failure 503 {object} models.APIResponse "identity_unavailable: the mobile number cannot be checked against the national ID now; the change stays, so confirm it again later"
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/contacts/changes/{id} [post]
func (h *UserHandler) ConfirmContactChange(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, i18n.M("UNAUTHORIZED"), nil)
		return
	}
	if !h.contactsReady(c) {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_CHANGE_ID"), nil)
		return
	}
	var req ContactChangeConfirmation
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, i18n.M("INVALID_INPUT"), nil)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	change, found, err := h.loadChange(ctx, id, userID.String())
	if err != nil {
		log.Printf("Failed to read a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM_THE_CHANGE"), nil)
		return
	}
	if !found {
		helpers.SendError(c, http.StatusNotFound, codeChangeNotFound, i18n.M("CHANGE_UNKNOWN_OR_EXPIRED"), nil)
		return
	}

	// Each code is checked once it is sent and not yet accepted. An
	// accepted code is marked in the change, so that a retry after a wrong
	// other code needs only the other one; the OTP store accepts a code
	// once, whichever instance asks.
	proofs := []struct {
		field, code, session, expected, mark string
		proven                               *bool
	}{
		{"account_code", req.AccountCode, change.AccountSession, change.AccountContact, "account_ok", &change.AccountProven},
		{"new_code", req.NewCode, change.NewSession, change.Value, "new_ok", &change.NewProven},
	}
	var invalid []string
	for _, p := range proofs {
		if *p.proven {
			continue
		}
		if p.code == "" {
			invalid = append(invalid, p.field)
			continue
		}
		got, err := h.ContactOTP.Verify(ctx, p.session, p.code)
		switch {
		case errors.Is(err, otp.ErrInvalidCode):
			invalid = append(invalid, p.field)
			continue
		case errors.Is(err, otp.ErrTooManyAttempts):
			h.RedisClient.Client.Del(ctx, h.changeKey(id))
			helpers.SendError(c, http.StatusTooManyRequests, codeRateLimited, i18n.M("TOO_MANY_WRONG_CODES"), nil)
			return
		case err != nil:
			log.Printf("Failed to verify a contact change code: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM_THE_CHANGE"), nil)
			return
		}
		if !strings.EqualFold(got, p.expected) {
			invalid = append(invalid, p.field)
			continue
		}
		*p.proven = true
		if err := h.RedisClient.Client.HSet(ctx, h.changeKey(id), p.mark, "1").Err(); err != nil {
			log.Printf("Failed to record an accepted contact change code: %v", err)
		}
	}
	if len(invalid) > 0 {
		h.recordContactAttempt(ctx, c, userID.String(), change, invalid)
		helpers.SendError(c, http.StatusUnauthorized, codeCodeInvalid, i18n.M("INVALID_OR_EXPIRED_CODE"), gin.H{"field": strings.Join(invalid, ",")})
		return
	}

	// Both proofs are in. The mobile number must belong to the national ID
	// of the user, when the user has one (#253). The provider is asked
	// before the change is claimed, so that an outage leaves the change to
	// be confirmed again, and outside the transaction, so that no row lock
	// waits for it.
	owned, err := h.checkOwnership(ctx, c, change)
	var refusal *errChange
	if errors.As(err, &refusal) && refusal.code == codeMobileOwnerMismatch {
		h.RedisClient.Client.Del(ctx, h.changeKey(id))
	}
	if sendRefusal(c, err) {
		return
	}
	if err != nil {
		log.Printf("Failed to check the ownership of a mobile number: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM_THE_CHANGE"), nil)
		return
	}
	// A contact that another account holds moves to this user, who proved
	// control of it, unless it still belongs to that account (#252).
	claimFrom, err := h.claimable(ctx, change)
	if sendRefusal(c, err) {
		return
	}
	if err != nil {
		log.Printf("Failed to check the account that holds a contact: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM_THE_CHANGE"), nil)
		return
	}

	// Exactly one confirmation applies the change: the one that deletes the
	// change from Redis.
	deleted, err := h.RedisClient.Client.Del(ctx, h.changeKey(id)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Failed to claim a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_CONFIRM_THE_CHANGE"), nil)
		return
	}
	if deleted == 0 {
		helpers.SendError(c, http.StatusNotFound, codeChangeNotFound, i18n.M("CHANGE_UNKNOWN_OR_EXPIRED"), nil)
		return
	}

	contacts, err := h.applyChange(ctx, c, change, owned, claimFrom)
	if sendRefusal(c, err) {
		return
	}
	if err != nil {
		log.Printf("Failed to apply a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("FAILED_TO_APPLY_THE_CHANGE"), nil)
		return
	}
	list := ContactList{Items: make([]Contact, 0, len(contacts))}
	for _, ct := range contacts {
		list.Items = append(list.Items, Contact{Type: ct.typ, Value: maskContact(ct.typ, ct.value), VerifiedAt: ct.verifiedAt.UTC()})
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("CONTACT_CHANGED_SUCCESSFULLY"), list)
}

// ownership is a successful check that a mobile number belongs to the
// national ID of its user (#253).
type ownership struct {
	nationalID, provider, ref string
}

// errIdentityUnavailable is the refusal of a mobile change that cannot be
// checked now.
var errIdentityUnavailable = &errChange{status: http.StatusServiceUnavailable, code: codeIdentityUnavailable,
	message: i18n.M("MOBILE_OWNERSHIP_CHECK_UNAVAILABLE")}

// errMobileRequired is the refusal of a removal of the mobile number of a
// user who has a verified national ID.
var errMobileRequired = &errChange{status: http.StatusConflict, code: codeMobileRequired,
	message: i18n.M("OWNED_MOBILE_REQUIRED")}

// nationalID returns the verified national ID of a user, and maps keys that
// are not loaded to errIdentityUnavailable.
func (h *UserHandler) nationalID(ctx context.Context, q identity.Querier, userHK string) (string, bool, error) {
	id, has, err := identity.NationalID(ctx, q, h.Sealer, userHK)
	if errors.Is(err, identity.ErrNoKeys) {
		log.Printf("A user has a national ID, but the data-protection keys are not loaded")
		return "", false, errIdentityUnavailable
	}
	return id, has, err
}

// checkOwnership checks a change of a mobile number of a user who has a
// verified national ID (#253): the new number must belong to it, and the
// number cannot be removed. It returns the check of the new number, or nil
// when none was needed. Users without a national ID are not checked; their
// number is checked when their national ID is verified (#158).
func (h *UserHandler) checkOwnership(ctx context.Context, c *gin.Context, ch pendingChange) (*ownership, error) {
	if ch.Type != notify.ContactMobile {
		return nil, nil
	}
	nationalID, has, err := h.nationalID(ctx, h.PostgresDB.DB, ch.UserID)
	if err != nil || !has {
		return nil, err
	}
	if ch.Action == contactRemove {
		return nil, errMobileRequired
	}
	if h.Ownership == nil {
		log.Printf("A mobile change of a user with a national ID cannot be checked: identity.mobile_ownership.provider is none")
		return nil, errIdentityUnavailable
	}
	checkCtx, cancel := context.WithTimeout(ctx, h.settings().Identity.MobileOwnership.Timeout())
	defer cancel()
	match, err := h.Ownership.Match(checkCtx, nationalID, ch.Value)
	if err != nil {
		// The errors of the providers never hold the number or the ID.
		log.Printf("The mobile ownership check with %s failed: %v", h.Ownership.Provider(), err)
		return nil, errIdentityUnavailable
	}
	if !match.Matched {
		e := contactEvent(c, ch.UserID, ch)
		e.Outcome, e.Reason = audit.Failure, codeMobileOwnerMismatch
		e.Changes = &audit.Changes{After: map[string]interface{}{
			ch.Type: maskContact(ch.Type, ch.Value), "ownership_provider": h.Ownership.Provider(), "ownership_ref": match.Ref,
		}}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := audit.Record(recordCtx, h.PostgresDB.DB, e); err != nil {
			log.Printf("Failed to record the audit event %s (failure): %v", e.Action, err)
		}
		return nil, &errChange{status: http.StatusForbidden, code: codeMobileOwnerMismatch,
			message: i18n.M("MOBILE_NOT_OWNED_BY_NATIONAL_ID")}
	}
	return &ownership{nationalID: nationalID, provider: h.Ownership.Provider(), ref: match.Ref}, nil
}

// errContactInUse is the refusal of a contact that another account holds
// and still owns. The user proved control of it, so the answer may say so.
var errContactInUse = &errChange{status: http.StatusConflict, code: codeContactInUse, message: i18n.M("CONTACT_BELONGS_TO_ANOTHER_ACCOUNT")}

// claimable returns the account that holds the new contact of a change
// and that the contact may move away from (#252), or "" when no other
// account holds it. The user proved control of the contact with a code. A
// mobile number of an account with a verified national ID moves only when
// the provider no longer matches it to that ID; one that still matches
// stays and is refused. An email address moves, since whoever controls it
// could sign in to that account anyway.
func (h *UserHandler) claimable(ctx context.Context, ch pendingChange) (string, error) {
	if ch.Action == contactRemove {
		return "", nil
	}
	owner, found, err := contactclaim.FindOwner(ctx, h.PostgresDB.DB, ch.Type, ch.Value)
	if err != nil || !found || owner.UserHK == ch.UserID {
		return "", err
	}
	if ch.Type != notify.ContactMobile {
		return owner.UserHK, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, h.settings().Identity.MobileOwnership.Timeout())
	defer cancel()
	verdict, _, err := contactclaim.Check(checkCtx, h.Ownership, h.Sealer, h.PostgresDB.DB, owner.UserHK, ch.Value)
	if errors.Is(err, identity.ErrUnavailable) {
		log.Printf("The ownership of a mobile number held by another account cannot be checked: %v", err)
		return "", errIdentityUnavailable
	}
	if err != nil {
		return "", err
	}
	if verdict == contactclaim.Owned {
		return "", errContactInUse
	}
	return owner.UserHK, nil
}

// recordContactAttempt records a confirmation with a wrong code. It is best
// effort, as the record of a failed sign-in is.
func (h *UserHandler) recordContactAttempt(ctx context.Context, c *gin.Context, userID string, ch pendingChange, invalid []string) {
	e := contactEvent(c, userID, ch)
	e.Outcome, e.Reason = audit.Failure, codeCodeInvalid
	e.Changes = &audit.Changes{After: map[string]interface{}{"invalid": strings.Join(invalid, ",")}}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := audit.Record(ctx, h.PostgresDB.DB, e); err != nil {
		log.Printf("Failed to record the audit event %s (failure): %v", e.Action, err)
	}
}

// contactEvent is the audit event of a change of a contact. It targets the
// user, and never holds a contact unmasked.
func contactEvent(c *gin.Context, userID string, ch pendingChange) audit.Event {
	e := audit.FromRequest(c)
	e.ActorType, e.ActorID = audit.ActorUser, userID
	e.Action = map[string]string{
		contactAdd: audit.ActionUserContactAdd, contactReplace: audit.ActionUserContactReplace, contactRemove: audit.ActionUserContactRemove,
	}[ch.Action]
	e.TargetType, e.TargetID = audit.TargetUser, userID
	return e
}

// applyChange applies a proven change in one transaction, under the lock of
// the user's row, after checking it again against the contacts as they are
// now. It returns the contacts after the change.
func (h *UserHandler) applyChange(ctx context.Context, c *gin.Context, ch pendingChange, owned *ownership, claimFrom string) ([]storedContact, error) {
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// A claim locks both accounts, in hk order, before anything else.
	if claimFrom != "" {
		if err := contactclaim.LockUsers(ctx, tx, ch.UserID, claimFrom); err != nil {
			return nil, err
		}
	}

	if _, err := loadProfile(ctx, tx, uuid.MustParse(ch.UserID), true); err != nil {
		if errors.Is(err, errAccountUnavailable) || errors.Is(err, errProfileNotFound) {
			return nil, &errChange{status: http.StatusForbidden, code: helpers.CodeAccountUnavailable, message: i18n.M("ACCOUNT_NOT_AVAILABLE_CONTACT_SUPPORT")}
		}
		return nil, err
	}
	contacts, now, err := verifiedContacts(ctx, tx, ch.UserID)
	if err != nil {
		return nil, err
	}
	// The contact that proved the account is checked by value, not by
	// type, in case it was replaced in between.
	plan, err := planChange(cooldownOf(h.settings()), ContactChangeRequest{Action: ch.Action, Type: ch.Type, Value: ch.Value, VerifyWith: accountType(contacts, ch.AccountContact)}, contacts, now)
	if err != nil {
		return nil, err
	}
	// The contact that proved the account must still be one of the user's.
	if _, ok := findContactValue(contacts, ch.AccountContact); !ok {
		return nil, &errChange{status: http.StatusConflict, code: codeContactNotFound, message: i18n.M("VERIFYING_CONTACT_CHANGED")}
	}
	// The national ID is read again under the lock: the check before must
	// have been made with the one the user has now (#253).
	if plan.typ == notify.ContactMobile {
		nationalID, has, err := h.nationalID(ctx, tx, ch.UserID)
		if err != nil {
			return nil, err
		}
		switch {
		case has && plan.action == contactRemove:
			return nil, errMobileRequired
		case has && (owned == nil || owned.nationalID != nationalID):
			return nil, &errChange{status: http.StatusConflict, code: codeIdentityChanged, message: i18n.M("IDENTITY_CHANGED_DURING_CHANGE")}
		case !has:
			owned = nil
		}
	}
	var checkedProvider, checkedRef any
	if owned != nil {
		checkedProvider, checkedRef = owned.provider, owned.ref
	}

	e := contactEvent(c, ch.UserID, ch)
	e.Outcome = audit.Success

	// The account that holds the new contact is read again under the
	// locks: only the one that was checked may lose it.
	claimed := false
	if plan.action != contactRemove {
		holder, found, err := contactclaim.FindOwner(ctx, tx, plan.typ, plan.value)
		if err != nil {
			return nil, err
		}
		if found && holder.UserHK != ch.UserID {
			if holder.UserHK != claimFrom {
				return nil, errContactInUse
			}
			origin := audit.FromRequest(c)
			origin.ActorType, origin.ActorID = audit.ActorUser, ch.UserID
			if _, claimed, err = contactclaim.Detach(ctx, tx, claimFrom, plan.typ, plan.value, contactclaim.ReasonClaimed, origin,
				time.Duration(h.settings().Security.ChangeHoldHours)*time.Hour); err != nil {
				return nil, err
			}
		}
	}
	before, _ := findContact(contacts, plan.typ)
	switch plan.action {
	case contactAdd:
		err = db.InsertContact(ctx, tx, ch.UserID, plan.typ, plan.value, true)
		if err == nil && owned != nil {
			_, err = tx.ExecContext(ctx, `UPDATE user_contacts c SET ownership_checked_at = NOW(), ownership_provider = $3, ownership_ref = $4
				WHERE user_hk = $1 AND type = $2::contact_type AND is_primary AND c.removed_at IS NULL`, ch.UserID, plan.typ, checkedProvider, checkedRef)
		}
		e.Changes = &audit.Changes{After: map[string]interface{}{plan.typ: maskContact(plan.typ, plan.value)}}
	case contactReplace:
		// A new number has not been checked, unless it was just now.
		_, err = tx.ExecContext(ctx, `UPDATE user_contacts c SET value = $3, verified_at = NOW(), updated_at = NOW(),
				ownership_checked_at = CASE WHEN $4::text IS NULL THEN NULL ELSE NOW() END, ownership_provider = $4, ownership_ref = $5
			WHERE user_hk = $1 AND type = $2::contact_type AND is_primary AND c.removed_at IS NULL`, ch.UserID, plan.typ, plan.value, checkedProvider, checkedRef)
		e.Changes = &audit.Changes{
			Before: map[string]interface{}{plan.typ: maskContact(plan.typ, before.value)},
			After:  map[string]interface{}{plan.typ: maskContact(plan.typ, plan.value)},
		}
	case contactRemove:
		// The row stays, marked, for the record (#248).
		_, err = tx.ExecContext(ctx, `UPDATE user_contacts c SET removed_at = NOW(), updated_at = NOW()
			WHERE user_hk = $1 AND type = $2::contact_type AND is_primary AND c.removed_at IS NULL`,
			ch.UserID, plan.typ)
		e.Changes = &audit.Changes{Before: map[string]interface{}{plan.typ: maskContact(plan.typ, before.value)}}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Another account took the contact after it was read; the user
		// may confirm again.
		return nil, errContactInUse
	}
	if err != nil {
		return nil, err
	}
	// Seal the contact rows that this transaction wrote (#139): their
	// updated_at is the transaction's NOW().
	if err := rowseal.Reseal(ctx, tx, rowseal.UserContacts, "user_hk = $1 AND updated_at = NOW()", ch.UserID); err != nil {
		return nil, err
	}
	// A change of a contact is a security change: it holds withdrawals
	// and the other operations that a second factor protects (#251), in
	// this transaction, so that the hold exists as soon as the change does.
	until, err := restrictions.StartSecurityHold(ctx, tx, ch.UserID, time.Duration(h.settings().Security.ChangeHoldHours)*time.Hour)
	if err != nil {
		return nil, err
	}
	if e.Changes.After == nil {
		e.Changes.After = map[string]interface{}{}
	}
	e.Changes.After["security_hold_until"] = until.UTC().Format(time.RFC3339)
	if claimed {
		e.Changes.After["claimed_from"] = claimFrom
	}
	if owned != nil {
		e.Changes.After["ownership_provider"], e.Changes.After["ownership_ref"] = owned.provider, owned.ref
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return nil, err
	}
	after, _, err := verifiedContacts(ctx, tx, ch.UserID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

// accountType returns the type of the contact with the given value, or ""
// when the user no longer has it.
func accountType(contacts []storedContact, value string) string {
	if ct, ok := findContactValue(contacts, value); ok {
		return ct.typ
	}
	return ""
}

func findContactValue(contacts []storedContact, value string) (storedContact, bool) {
	for _, ct := range contacts {
		if strings.EqualFold(ct.value, value) {
			return ct, true
		}
	}
	return storedContact{}, false
}
