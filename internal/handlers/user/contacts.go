package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/clientip"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"

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
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	contacts, err := verifiedContacts(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to list the contacts of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to list contacts.", nil)
		return
	}
	list := ContactList{Items: make([]Contact, 0, len(contacts))}
	for _, ct := range contacts {
		list.Items = append(list.Items, Contact{Type: ct.typ, Value: maskContact(ct.typ, ct.value), VerifiedAt: ct.verifiedAt.UTC()})
	}
	helpers.SendResponse(c, http.StatusOK, "Contacts retrieved successfully.", list)
}

// storedContact is a verified contact as the database holds it.
type storedContact struct {
	typ, value string
	verifiedAt time.Time
}

// verifiedContacts returns the verified email and mobile contacts of a user,
// email first.
func verifiedContacts(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}, userHK string) ([]storedContact, error) {
	rows, err := q.QueryContext(ctx, `SELECT type::text, value, verified_at FROM user_contacts c
		WHERE user_hk = $1 AND is_primary AND verified_at IS NOT NULL AND type IN ('email', 'mobile') AND c.removed_at IS NULL
		ORDER BY type`, userHK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedContact
	for rows.Next() {
		var ct storedContact
		if err := rows.Scan(&ct.typ, &ct.value, &ct.verifiedAt); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, rows.Err()
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

// errChange is a refusal of a change, with its status, code, and message.
type errChange struct {
	status        int
	code, message string
}

func (e *errChange) Error() string { return e.message }

// planChange checks a change against the current contacts of the user. It
// runs when the change starts, and again under the row lock when it is
// applied, since the contacts may have changed in between.
func planChange(req ContactChangeRequest, contacts []storedContact) (contactChange, error) {
	ch := contactChange{action: req.Action, typ: req.Type, value: req.Value}
	current, has := findContact(contacts, ch.typ)
	switch ch.action {
	case contactAdd:
		if has {
			return ch, &errChange{http.StatusConflict, codeContactExists, "You already have a contact of this type; replace it instead."}
		}
	case contactReplace:
		if !has {
			return ch, &errChange{http.StatusConflict, codeContactNotFound, "You have no contact of this type to replace."}
		}
		if strings.EqualFold(current.value, ch.value) {
			return ch, &errChange{http.StatusConflict, codeSameContact, "The new contact is the one you already have."}
		}
	case contactRemove:
		if !has {
			return ch, &errChange{http.StatusConflict, codeContactNotFound, "You have no contact of this type to remove."}
		}
		if len(contacts) < 2 {
			return ch, &errChange{http.StatusConflict, codeLastContact, "Your last verified contact cannot be removed."}
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
		return ch, &errChange{http.StatusConflict, codeContactNotFound, "The contact to verify with is not one of your verified contacts."}
	}
	ch.account = account
	return ch, nil
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
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, "Validation failed", gin.H{"errors": errs})
}

// contactsReady reports whether the contact routes have what they need.
func (h *UserHandler) contactsReady(c *gin.Context) bool {
	if h.ContactOTP == nil || h.OTPSender == nil || h.PostgresDB == nil || h.RedisClient == nil {
		helpers.SendResponse(c, http.StatusServiceUnavailable, "Contact changes are not available.", nil)
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
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/contacts/changes [post]
func (h *UserHandler) StartContactChange(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	if !h.contactsReady(c) {
		return
	}
	var req ContactChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
		return
	}
	if errs := validateChangeRequest(&req); errs != nil {
		sendContactValidation(c, errs)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	contacts, err := verifiedContacts(ctx, h.PostgresDB.DB, userID.String())
	if err != nil {
		log.Printf("Failed to read the contacts of a user: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to start the change.", nil)
		return
	}
	plan, err := planChange(req, contacts)
	if sendRefusal(c, err) {
		return
	}
	for _, typ := range []string{plan.account.typ, plan.newType()} {
		if typ != "" && !h.OTPSender.Supports(typ) {
			helpers.SendError(c, http.StatusConflict, codeChannelMissing, fmt.Sprintf("Codes cannot be sent by %s.", typ), nil)
			return
		}
	}

	// A contact that belongs to another user is not refused here, so that
	// the answer does not tell whether it is taken; the code proves control
	// of it first, and only then may the refusal say so.
	client := clientip.Key(c.ClientIP(), config.Config.RateLimits.IPv6PrefixLength)
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
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to start the change.", nil)
		return
	}
	helpers.SendResponse(c, http.StatusOK, "Codes sent. Confirm the change with them.", started)
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
		helpers.SendError(c, refusal.status, refusal.code, refusal.message, nil)
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
		helpers.SendError(c, http.StatusTooManyRequests, codeRateLimited, "Too many codes requested. Please try again later.", nil)
	default:
		log.Printf("Failed to send a contact change code: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to send the code.", nil)
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
// @Description Applies the change once both codes are right. A wrong code answers 401 code_invalid and names it in info.field; the codes already accepted stay accepted, so a retry needs only the others. Codes expire with the change.
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
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/user/contacts/changes/{id} [post]
func (h *UserHandler) ConfirmContactChange(c *gin.Context) {
	userID, ok := callerID(c)
	if !ok {
		helpers.SendResponse(c, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	if !h.contactsReady(c) {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid UUID format for the change", nil)
		return
	}
	var req ContactChangeConfirmation
	if err := c.ShouldBindJSON(&req); err != nil {
		helpers.SendResponse(c, http.StatusBadRequest, "Invalid input: Please check the provided data format.", nil)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	change, found, err := h.loadChange(ctx, id, userID.String())
	if err != nil {
		log.Printf("Failed to read a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to confirm the change.", nil)
		return
	}
	if !found {
		helpers.SendError(c, http.StatusNotFound, codeChangeNotFound, "The change is unknown or has expired; start it again.", nil)
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
			helpers.SendError(c, http.StatusTooManyRequests, codeRateLimited, "Too many wrong codes. Start the change again later.", nil)
			return
		case err != nil:
			log.Printf("Failed to verify a contact change code: %v", err)
			helpers.SendResponse(c, http.StatusInternalServerError, "Failed to confirm the change.", nil)
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
		helpers.SendError(c, http.StatusUnauthorized, codeCodeInvalid, "Invalid or expired code.", gin.H{"field": strings.Join(invalid, ",")})
		return
	}

	// Both proofs are in. Exactly one confirmation applies the change: the
	// one that deletes the change from Redis.
	deleted, err := h.RedisClient.Client.Del(ctx, h.changeKey(id)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("Failed to claim a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to confirm the change.", nil)
		return
	}
	if deleted == 0 {
		helpers.SendError(c, http.StatusNotFound, codeChangeNotFound, "The change is unknown or has expired; start it again.", nil)
		return
	}

	contacts, err := h.applyChange(ctx, c, change)
	if sendRefusal(c, err) {
		return
	}
	if err != nil {
		log.Printf("Failed to apply a contact change: %v", err)
		helpers.SendResponse(c, http.StatusInternalServerError, "Failed to apply the change.", nil)
		return
	}
	list := ContactList{Items: make([]Contact, 0, len(contacts))}
	for _, ct := range contacts {
		list.Items = append(list.Items, Contact{Type: ct.typ, Value: maskContact(ct.typ, ct.value), VerifiedAt: ct.verifiedAt.UTC()})
	}
	helpers.SendResponse(c, http.StatusOK, "Contact changed successfully.", list)
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
func (h *UserHandler) applyChange(ctx context.Context, c *gin.Context, ch pendingChange) ([]storedContact, error) {
	tx, err := h.PostgresDB.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := loadProfile(ctx, tx, uuid.MustParse(ch.UserID), true); err != nil {
		if errors.Is(err, errAccountUnavailable) || errors.Is(err, errProfileNotFound) {
			return nil, &errChange{http.StatusForbidden, helpers.CodeAccountUnavailable, "This account is not available. Please contact support."}
		}
		return nil, err
	}
	contacts, err := verifiedContacts(ctx, tx, ch.UserID)
	if err != nil {
		return nil, err
	}
	plan, err := planChange(ContactChangeRequest{Action: ch.Action, Type: ch.Type, Value: ch.Value}, contacts)
	if err != nil {
		return nil, err
	}
	// The contact that proved the account must still be one of the user's.
	if _, ok := findContactValue(contacts, ch.AccountContact); !ok {
		return nil, &errChange{http.StatusConflict, codeContactNotFound, "The contact you verified with has changed; start the change again."}
	}

	e := contactEvent(c, ch.UserID, ch)
	e.Outcome = audit.Success
	before, _ := findContact(contacts, plan.typ)
	switch plan.action {
	case contactAdd:
		err = db.InsertContact(ctx, tx, ch.UserID, plan.typ, plan.value, true)
		e.Changes = &audit.Changes{After: map[string]interface{}{plan.typ: maskContact(plan.typ, plan.value)}}
	case contactReplace:
		_, err = tx.ExecContext(ctx, `UPDATE user_contacts c SET value = $3, verified_at = NOW(), updated_at = NOW()
			WHERE user_hk = $1 AND type = $2::contact_type AND is_primary AND c.removed_at IS NULL`, ch.UserID, plan.typ, plan.value)
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
		// The user proved control of the contact, so the answer may say
		// that another account holds it; that account signs in with it.
		return nil, &errChange{http.StatusConflict, codeContactInUse, "This contact belongs to another account."}
	}
	if err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return nil, err
	}
	after, err := verifiedContacts(ctx, tx, ch.UserID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

func findContactValue(contacts []storedContact, value string) (storedContact, bool) {
	for _, ct := range contacts {
		if strings.EqualFold(ct.value, value) {
			return ct, true
		}
	}
	return storedContact{}, false
}
