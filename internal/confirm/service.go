package confirm

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
)

// Codes issues and checks the codes sent by SMS and email: an *otp.Store
// in a namespace of its own, so that a confirmation code cannot sign in
// and a sign-in code cannot confirm.
type Codes interface {
	Issue(ctx context.Context, contact, client string) (otp.Issued, error)
	Verify(ctx context.Context, sessionID, code string) (string, error)
	TTL() time.Duration
}

// Authenticator checks the user's authenticator app: *userauth.TOTP. Each
// code is accepted once, and wrong codes lock it for a while.
type Authenticator interface {
	Enabled(ctx context.Context, userHK string) (bool, error)
	Verify(ctx context.Context, userHK, code string) error
}

// Passkeys proves the user's passkeys: *passkey.Service. A ceremony begun
// for one scope finishes no other, and the device must verify its holder.
type Passkeys interface {
	Enabled() bool
	Count(ctx context.Context, userHK string) (int, error)
	BeginAssertion(ctx context.Context, userHK, scope string) (*protocol.CredentialAssertion, error)
	FinishAssertion(ctx context.Context, userHK, scope string, response []byte) error
}

// passkeyScope is the scope of the passkey ceremony of a confirmation, so
// that the answer to one confirmation's challenge confirms no other.
func passkeyScope(id string) string { return "confirmation:" + id }

// Service confirms operations with the user's second factor.
type Service struct {
	DB *sql.DB
	// Passkeys proves passkeys (#373); nil is no such method.
	Passkeys Passkeys
	// Codes and Sender issue and deliver the codes of SMS and email; nil
	// is no such method.
	Codes  Codes
	Sender notify.OTPSender
	// AntiPhishing reads the anti-phishing code that the emails show
	// (#375); nil shows none.
	AntiPhishing antiphishing.Reader
	// SMSForWithdrawals reports that the SMS provider has a template for
	// the codes that confirm withdrawals. Without one, SMS is not offered:
	// a withdrawal code never goes out with the sign-in text.
	SMSForWithdrawals bool
	// Authenticator checks authenticator apps; nil is no such method.
	Authenticator Authenticator
	// MaxAttempts bounds the wrong codes of one confirmation; 5 when 0.
	MaxAttempts int
	// Links issue the "this was not me" link of each message of a code
	// (#374); nil sends none.
	Links DenyLinks
}

// DenyLinks issues the "this was not me" link of a message of a code
// (#374): denylink.Service. Each message gets a link of its own.
type DenyLinks interface {
	Issue(ctx context.Context, userHK, confirmationID string) (Link, error)
}

// Link is a "this was not me" link: its token, which an SMS template
// places after the exchange's own domain, and the whole URL, which an
// email shows.
type Link struct{ Token, URL string }

// DefaultTTL is how long a confirmation lasts without a code store.
const DefaultTTL = 5 * time.Minute

// ttl is how long a confirmation waits for its code, and then for its
// operation: the lifetime of the codes.
func (s Service) ttl() time.Duration {
	if s.Codes != nil {
		return s.Codes.TTL()
	}
	return DefaultTTL
}

func (s Service) maxAttempts() int {
	if s.MaxAttempts <= 0 {
		return 5
	}
	return s.MaxAttempts
}

// contactOf is a verified contact that a code can be sent to.
type contactOf struct{ typ, value, masked string }

// available returns the methods the user can use now, with the contacts
// that the codes of SMS and email go to.
func (s Service) available(ctx context.Context, userHK string) (map[Method]contactOf, error) {
	out := map[Method]contactOf{}
	if s.Passkeys != nil && s.Passkeys.Enabled() {
		n, err := s.Passkeys.Count(ctx, userHK)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out[MethodPasskey] = contactOf{}
		}
	}
	if s.Authenticator != nil {
		enabled, err := s.Authenticator.Enabled(ctx, userHK)
		switch {
		case errors.Is(err, rowseal.ErrUnverified):
			// An authenticator turned on in SQL is not offered (#337).
		case err != nil:
			return nil, err
		case enabled:
			out[MethodTOTP] = contactOf{}
		}
	}
	if s.Codes == nil || s.Sender == nil {
		return out, nil
	}
	contacts, err := s.contacts(ctx, userHK)
	if err != nil {
		return nil, err
	}
	for _, c := range contacts {
		method := MethodEmail
		if c.typ == "mobile" {
			if !s.SMSForWithdrawals {
				continue
			}
			method = MethodSMS
		}
		out[method] = c
	}
	return out, nil
}

// contacts returns the verified primary email address and mobile number of
// the user that the sender reaches.
func (s Service) contacts(ctx context.Context, userHK string) ([]contactOf, error) {
	//nolint:gosec // G202: the columns are constants of rowseal; the value is an argument.
	rows, err := s.DB.QueryContext(ctx, `SELECT id, type::text, value, value_enc, `+rowseal.UserContacts.Fields("c")+`
		FROM user_contacts c WHERE user_hk = $1 AND is_primary AND verified_at IS NOT NULL AND removed_at IS NULL
			AND type IN ('email', 'mobile')`, userHK)
	if err != nil {
		return nil, fmt.Errorf("read the contacts: %w", err)
	}
	type read struct {
		id    int64
		typ   string
		plain *string
		enc   []byte
		row   *rowseal.Row
	}
	var all []read
	for rows.Next() {
		r := read{row: rowseal.UserContacts.NewRow()}
		if err := rows.Scan(append([]any{&r.id, &r.typ, &r.plain, &r.enc}, r.row.Dest()...)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read a contact: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the contacts: %w", err)
	}
	var out []contactOf
	for _, r := range all {
		// A contact changed in SQL, such as an address swapped for an
		// attacker's, receives no code (#139).
		if err := rowseal.Check(ctx, rowseal.UserContacts, r.row); err != nil {
			if errors.Is(err, rowseal.ErrUnverified) {
				continue
			}
			return nil, err
		}
		if !s.Sender.Supports(r.typ) {
			continue
		}
		value, err := db.OpenContact(r.id, r.plain, r.enc)
		if err != nil {
			return nil, err
		}
		out = append(out, contactOf{typ: r.typ, value: value, masked: notify.MaskContact(r.typ, value)})
	}
	return out, nil
}

// Settings are the methods of a user: the one they chose, the one that
// confirmations use, and every one they can use now.
type Settings struct {
	// Method is the method the user chose; absent when they chose none.
	Method Method `json:"method,omitempty" enums:"passkey,totp,sms,email"`
	// Effective is the method that confirmations use: the chosen one
	// while it is available, or else the first available of totp, sms,
	// and email. Absent when none is available.
	Effective Method `json:"effective,omitempty" enums:"passkey,totp,sms,email"`
	// Available are the methods the user can use now.
	Available []Method `json:"available"`
}

// chosen reads the method the user chose, or "" for none. A choice that
// does not match its seal counts as none, and its finding is recorded.
func (s Service) chosen(ctx context.Context, userHK string) (Method, error) {
	var method string
	row := rowseal.UserConfirmationMethods.NewRow()
	err := s.DB.QueryRowContext(ctx, `SELECT method, `+rowseal.UserConfirmationMethods.Fields("")+`
		FROM user_confirmation_methods WHERE user_hk = $1`, userHK).Scan(append([]any{&method}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the method: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.UserConfirmationMethods, row); err != nil {
		if errors.Is(err, rowseal.ErrUnverified) {
			return "", nil
		}
		return "", err
	}
	return Method(method), nil
}

func (s Service) settings(ctx context.Context, userHK string) (Settings, map[Method]contactOf, error) {
	chosen, err := s.chosen(ctx, userHK)
	if err != nil {
		return Settings{}, nil, err
	}
	available, err := s.available(ctx, userHK)
	if err != nil {
		return Settings{}, nil, err
	}
	st := Settings{Method: chosen, Available: []Method{}}
	for _, m := range Methods {
		if _, ok := available[m]; ok {
			st.Available = append(st.Available, m)
		}
	}
	if _, ok := available[chosen]; ok {
		st.Effective = chosen
	} else if len(st.Available) > 0 {
		st.Effective = st.Available[0]
	}
	return st, available, nil
}

// Settings returns the methods of the user.
func (s Service) Settings(ctx context.Context, userHK string) (Settings, error) {
	st, _, err := s.settings(ctx, userHK)
	return st, err
}

// SetMethod records the method the user chooses, which must be available
// to them, and origin, the audit event of the request.
func (s Service) SetMethod(ctx context.Context, userHK string, m Method, origin audit.Event) error {
	if !m.Valid() {
		return ErrMethodUnavailable
	}
	st, _, err := s.settings(ctx, userHK)
	if err != nil {
		return err
	}
	if !contains(st.Available, m) {
		return ErrMethodUnavailable
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_confirmation_methods (user_hk, method) VALUES ($1, $2)
		ON CONFLICT (user_hk) DO UPDATE SET method = EXCLUDED.method, updated_at = NOW()`, userHK, string(m)); err != nil {
		return fmt.Errorf("record the method: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserConfirmationMethods, "user_hk = $1", userHK); err != nil {
		return err
	}
	event := origin
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.Action, event.TargetType, event.TargetID = audit.ActionUserConfirmationMethodSet, audit.TargetUser, userHK
	event.Outcome = audit.Success
	event.Changes = &audit.Changes{Before: map[string]interface{}{"method": st.Method}, After: map[string]interface{}{"method": m}}
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func contains(methods []Method, m Method) bool {
	for _, x := range methods {
		if x == m {
			return true
		}
	}
	return false
}

// Details are what a code confirms, as the user is told: the amount and
// the destination, masked, never a full number. Each is at most 25
// characters, so that it fits a parameter of an SMS template (#371).
type Details struct {
	// Amount is the amount with its currency, such as "100 USDT".
	Amount string `json:"amount" example:"100 USDT"`
	// Destination is where it goes, masked, such as "TR7NHq...gjLj6t".
	Destination string `json:"destination" example:"TR7NHq...gjLj6t"`
	// Fee is the network fee that is charged on top of the amount, with its
	// currency, such as "1 USDT" (#408). Empty when the operation has none.
	// It is stored with the confirmation and sealed, so that every message
	// of the code, the first and one sent again, shows it.
	Fee string `json:"fee,omitempty" example:"1 USDT"`
}

// String is the details in one line, for an email or a template that has
// one parameter.
func (d Details) String() string {
	if d.Fee != "" {
		return d.Amount + " to " + d.Destination + " (fee " + d.Fee + ")"
	}
	return d.Amount + " to " + d.Destination
}

// MaxDetailLength bounds the amount and the destination of Details.
const MaxDetailLength = 25

func (d Details) valid() bool {
	return d.Amount != "" && d.Destination != "" && len(d.Amount) <= MaxDetailLength && len(d.Destination) <= MaxDetailLength && len(d.Fee) <= MaxDetailLength
}

// Challenge is a confirmation as the user sees it.
type Challenge struct {
	ID     string `json:"id" format:"uuid"`
	Method Method `json:"method" enums:"passkey,totp,sms,email"`
	// SentTo is the contact that an SMS or email code went to, masked.
	SentTo string `json:"sent_to,omitempty" example:"091*****567"`
	// Passkey are the options for navigator.credentials.get of a
	// confirmation by passkey, under publicKey. The device's answer goes to
	// POST /user/confirmations/{id} as passkey.
	Passkey *PasskeyOptions `json:"passkey,omitempty"`
	// Details are what the code confirms.
	Details   Details   `json:"details"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PasskeyOptions are the options of a confirmation by passkey that the
// client hands to navigator.credentials.get, as passkey.Options documents
// them.
type PasskeyOptions struct {
	// PublicKey holds the options, as the WebAuthn API defines them.
	PublicKey map[string]any `json:"publicKey"`
}

// optionsOf returns the options of an assertion in the form the API
// documents.
func optionsOf(a *protocol.CredentialAssertion) (*PasskeyOptions, error) {
	raw, err := json.Marshal(a.Response)
	if err != nil {
		return nil, fmt.Errorf("encode the passkey options: %w", err)
	}
	out := &PasskeyOptions{}
	if err := json.Unmarshal(raw, &out.PublicKey); err != nil {
		return nil, fmt.Errorf("encode the passkey options: %w", err)
	}
	return out, nil
}

// Proof is what confirms a confirmation: a code, or the answer of a
// device to the options of a confirmation by passkey.
type Proof struct {
	Code    string
	Passkey []byte
}

// Start starts the confirmation of an operation of the user: purpose, its
// digest, and details, what the user is told it confirms. It sends the code
// by SMS or email, or expects one of the authenticator, by the user's
// method. client is the requester, for the limits of the code store.
func (s Service) Start(ctx context.Context, userHK string, p Purpose, digest []byte, details Details, client string) (Challenge, error) {
	if len(digest) != sha256Size || !details.valid() {
		return Challenge{}, fmt.Errorf("a confirmation of %d bytes for %q", len(digest), details)
	}
	st, available, err := s.settings(ctx, userHK)
	if err != nil {
		return Challenge{}, err
	}
	if st.Effective == "" {
		return Challenge{}, ErrNoMethod
	}
	c := Challenge{ID: uuid.NewString(), Method: st.Effective, Details: details}
	var session *string
	var code string
	contact := available[st.Effective]
	if st.Effective == MethodPasskey {
		options, err := s.Passkeys.BeginAssertion(ctx, userHK, passkeyScope(c.ID))
		if err != nil {
			return Challenge{}, fmt.Errorf("begin the passkey: %w", err)
		}
		if c.Passkey, err = optionsOf(options); err != nil {
			return Challenge{}, err
		}
	}
	if st.Effective.sendsCode() {
		issued, err := s.Codes.Issue(ctx, contact.value, client)
		if errors.Is(err, otp.ErrTooManyRequests) || errors.Is(err, otp.ErrTooManyAttempts) {
			return Challenge{}, fmt.Errorf("%w: %w", ErrTooManyAttempts, err)
		}
		if err != nil {
			return Challenge{}, fmt.Errorf("issue the code: %w", err)
		}
		session, code, c.SentTo = &issued.SessionID, issued.Code, contact.masked
	}
	var sentTo *string
	if c.SentTo != "" {
		sentTo = &c.SentTo
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `INSERT INTO operation_confirmations
			(id, user_hk, purpose, digest, amount, destination, fee, method, code_session, sent_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, NOW() + make_interval(secs => $11)) RETURNING expires_at`,
		c.ID, userHK, string(p), digest, details.Amount, details.Destination, details.Fee, string(c.Method), session, sentTo,
		s.ttl().Seconds()).Scan(&c.ExpiresAt); err != nil {
		return Challenge{}, fmt.Errorf("record the confirmation: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", c.ID); err != nil {
		return Challenge{}, err
	}
	if err := tx.Commit(); err != nil {
		return Challenge{}, err
	}
	if session != nil {
		if err := s.send(ctx, userHK, c.ID, p, contact, code, details); err != nil {
			return Challenge{}, err
		}
	}
	return c, nil
}

const sha256Size = 32

// send sends a code of the confirmation id to a verified contact of
// userHK, with the "this was not me" link of the message (#374). An email
// shows the user's anti-phishing code (#375).
func (s Service) send(ctx context.Context, userHK, id string, p Purpose, to contactOf, code string, details Details) error {
	purpose := ""
	if p == PurposeWithdrawal {
		purpose = notify.PurposeWithdrawal
	}
	msg := notify.OTPMessage{ContactType: to.typ, Contact: to.value, MaskedContact: to.masked,
		Code: code, Purpose: purpose, Details: details.String(), Amount: details.Amount, Destination: details.Destination,
		Fee: details.Fee}
	if s.Links != nil {
		link, err := s.Links.Issue(ctx, userHK, id)
		if err != nil {
			// A code never goes out without its link; the user can ask
			// for it again.
			return fmt.Errorf("issue the link of the message: %w", err)
		}
		msg.DenyToken, msg.DenyLink = link.Token, link.URL
	}
	if s.AntiPhishing != nil {
		var err error
		if msg.AntiPhishingCode, err = s.AntiPhishing.ShowTo(ctx, userHK, to.typ, to.value); err != nil {
			// The code still goes out, without the anti-phishing code.
			log.Printf("Failed to read the anti-phishing code of user %s for a confirmation: %v", userHK, err)
			msg.AntiPhishingCode = ""
		}
	}
	return s.Sender.SendOTP(ctx, msg)
}

// stored is a confirmation as it is stored.
type stored struct {
	id, userHK, purpose, method string
	details                     Details
	digest                      []byte
	session, sentTo             sql.NullString
	failed                      int
	expiresAt                   time.Time
	confirmed, consumed         bool
	expired                     bool
	// usable is whether a confirmed confirmation may still be consumed.
	usable bool
	// denied is a confirmation that its user denied with the link of its
	// message (#374).
	denied bool
}

// load reads the user's confirmation id in tx, locked, and checks its seal.
func (s Service) load(ctx context.Context, tx *sql.Tx, userHK, id string) (stored, error) {
	return s.read(ctx, tx, userHK, id, true)
}

// Bound returns what the confirmation id of the user confirms, as the user
// was told it, and whether it is bound to digest under p, in tx, without
// consuming it. The withdrawals use it to tell a confirmation whose fee
// changed from one of another request (#460). A confirmation that does not
// match its seal is refused: the error wraps ErrUnverified.
func (s Service) Bound(ctx context.Context, tx *sql.Tx, userHK, id string, p Purpose, digest []byte) (Details, bool, error) {
	c, err := s.read(ctx, tx, userHK, id, false)
	if err != nil {
		return Details{}, false, err
	}
	return c.details, c.purpose == string(p) && subtle.ConstantTimeCompare(c.digest, digest) == 1, nil
}

// rowReader reads one row: the database or a transaction.
type rowReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// peek reads the confirmation id of the user without a lock, on a
// connection of its own.
func (s Service) peek(ctx context.Context, userHK, id string) (stored, error) {
	return s.read(ctx, s.DB, userHK, id, false)
}

// read reads the confirmation id of the user in q, and locks it when lock
// is set.
func (s Service) read(ctx context.Context, q rowReader, userHK, id string, lock bool) (stored, error) {
	if _, err := uuid.Parse(id); err != nil {
		return stored{}, ErrNotFound
	}
	var c stored
	row := rowseal.OperationConfirmations.NewRow()
	query := `SELECT id::text, user_hk::text, purpose, amount, destination, COALESCE(fee, ''), method, digest, code_session, sent_to,
			failed_attempts, expires_at, confirmed_at IS NOT NULL, consumed_at IS NOT NULL, expires_at <= NOW(),
			COALESCE(confirmed_at + make_interval(secs => $3) > NOW(), false), denied_at IS NOT NULL, ` +
		rowseal.OperationConfirmations.Fields("") + `
		FROM operation_confirmations WHERE id = $1 AND user_hk = $2`
	if lock {
		query += ` FOR UPDATE`
	}
	err := q.QueryRowContext(ctx, query, id, userHK, s.ttl().Seconds()).
		Scan(append([]any{&c.id, &c.userHK, &c.purpose, &c.details.Amount, &c.details.Destination, &c.details.Fee, &c.method, &c.digest, &c.session, &c.sentTo,
			&c.failed, &c.expiresAt, &c.confirmed, &c.consumed, &c.expired, &c.usable, &c.denied}, row.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return stored{}, ErrNotFound
	}
	if err != nil {
		return stored{}, fmt.Errorf("read the confirmation: %w", err)
	}
	if err := rowseal.Check(ctx, rowseal.OperationConfirmations, row); err != nil {
		return stored{}, err
	}
	return c, nil
}

// Confirm checks the user's proof of the confirmation id, a code or a
// passkey's answer, and records origin, the audit event of the request,
// with the outcome. A wrong proof counts against the confirmation: after
// MaxAttempts, it can no longer be confirmed.
//
// The proof is checked before the confirmation is locked: the passkeys,
// the authenticator, and the code store read and write on connections of
// their own, and a transaction that waited for them would hold a second
// connection of the pool, so that enough requests at once would exhaust it
// (#405). The lock then checks again that nothing changed meanwhile.
func (s Service) Confirm(ctx context.Context, userHK, id string, proof Proof, origin audit.Event) error {
	c, err := s.peek(ctx, userHK, id)
	if err != nil {
		return err
	}
	if err := s.confirmable(c); err != nil {
		return err
	}
	wrong, err := s.check(ctx, userHK, c, proof)
	if err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked, err := s.load(ctx, tx, userHK, id)
	if err != nil {
		return err
	}
	if err := s.confirmable(locked); err != nil {
		return err
	}
	if locked.method != c.method || locked.session != c.session {
		// A new code or ceremony was sent meanwhile: the proof was of the
		// one before.
		return ErrInvalidCode
	}
	c = locked
	event := origin
	event.ActorType, event.ActorID = audit.ActorUser, userHK
	event.Action, event.TargetType, event.TargetID = audit.ActionUserOperationConfirm, audit.TargetOperationConfirmation, id
	event.Changes = &audit.Changes{After: map[string]interface{}{"purpose": c.purpose, "method": c.method, "amount": c.details.Amount, "destination": c.details.Destination}}

	if wrong != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE operation_confirmations SET failed_attempts = failed_attempts + 1 WHERE id = $1`, id); err != nil {
			return fmt.Errorf("count the attempt: %w", err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", id); err != nil {
			return err
		}
		event.Outcome, event.Reason = audit.Failure, "invalid_code"
		if errors.Is(wrong, ErrTooManyAttempts) {
			event.Reason = "too_many_attempts"
		}
		if err := audit.Record(ctx, tx, event); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if c.failed+1 >= s.maxAttempts() {
			return ErrTooManyAttempts
		}
		return wrong
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operation_confirmations SET confirmed_at = NOW() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("record the confirmation: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", id); err != nil {
		return err
	}
	event.Outcome = audit.Success
	if err := audit.Record(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// confirmable refuses a confirmation that can no longer be confirmed.
func (s Service) confirmable(c stored) error {
	switch {
	case c.denied:
		return ErrDenied
	case c.confirmed:
		return ErrAlreadyConfirmed
	case c.expired:
		return ErrExpired
	case c.failed >= s.maxAttempts():
		return ErrTooManyAttempts
	}
	return nil
}

// check checks the proof with the confirmation's method. It returns a
// wrong proof as wrong, ErrInvalidCode or ErrTooManyAttempts, and any other
// failure as err.
func (s Service) check(ctx context.Context, userHK string, c stored, proof Proof) (wrong, err error) {
	switch Method(c.method) {
	case MethodPasskey:
		if s.Passkeys == nil || len(proof.Passkey) == 0 {
			return ErrInvalidCode, nil
		}
		err := s.Passkeys.FinishAssertion(ctx, userHK, passkeyScope(c.id), proof.Passkey)
		switch {
		case err == nil:
			return nil, nil
		case errors.Is(err, passkey.ErrInvalid), errors.Is(err, passkey.ErrNoCeremony), errors.Is(err, passkey.ErrCloned):
			// The ceremony is taken by the attempt: a new one needs
			// Resend, which begins it again.
			return ErrInvalidCode, nil
		case errors.Is(err, passkey.ErrDisabled), errors.Is(err, passkey.ErrNoPasskey):
			return nil, ErrMethodUnavailable
		}
		return nil, err
	case MethodTOTP:
		if s.Authenticator == nil {
			return nil, ErrMethodUnavailable
		}
		err := s.Authenticator.Verify(ctx, userHK, proof.Code)
		var locked *userauth.LockedError
		switch {
		case err == nil:
			return nil, nil
		case errors.Is(err, userauth.ErrWrongTOTP):
			return ErrInvalidCode, nil
		case errors.As(err, &locked):
			return ErrTooManyAttempts, nil
		case errors.Is(err, userauth.ErrNoTOTP):
			return nil, ErrMethodUnavailable
		}
		return nil, err
	}
	if s.Codes == nil || !c.session.Valid {
		return nil, ErrMethodUnavailable
	}
	_, err = s.Codes.Verify(ctx, c.session.String, proof.Code)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, otp.ErrInvalidCode):
		return ErrInvalidCode, nil
	case errors.Is(err, otp.ErrTooManyAttempts):
		return ErrTooManyAttempts, nil
	}
	return nil, err
}

// Resend sends a new code of an SMS or email confirmation that is not
// confirmed yet, or begins a new passkey ceremony of a confirmation by
// passkey, and gives it a new lifetime. The code store's limits bound how
// often codes go out. An authenticator confirmation has nothing to send.
//
// What it sends is prepared before the confirmation is locked, since the
// passkeys, the contacts, and the code store are read on connections of
// their own (#405); the lock then checks again that the confirmation is
// still open.
func (s Service) Resend(ctx context.Context, userHK, id, client string) (Challenge, error) {
	c, err := s.peek(ctx, userHK, id)
	if err != nil {
		return Challenge{}, err
	}
	if err := s.resendable(c); err != nil {
		return Challenge{}, err
	}
	if Method(c.method) == MethodPasskey {
		return s.restartPasskey(ctx, userHK, c)
	}
	available, err := s.available(ctx, userHK)
	if err != nil {
		return Challenge{}, err
	}
	contact, ok := available[Method(c.method)]
	if !ok {
		return Challenge{}, ErrMethodUnavailable
	}
	issued, err := s.Codes.Issue(ctx, contact.value, client)
	if errors.Is(err, otp.ErrTooManyRequests) || errors.Is(err, otp.ErrTooManyAttempts) {
		return Challenge{}, fmt.Errorf("%w: %w", ErrTooManyAttempts, err)
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("issue the code: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback()
	if c, err = s.relock(ctx, tx, userHK, c); err != nil {
		return Challenge{}, err
	}
	out := Challenge{ID: c.id, Method: Method(c.method), SentTo: contact.masked, Details: c.details}
	if err := tx.QueryRowContext(ctx, `UPDATE operation_confirmations SET code_session = $2, sent_to = $3,
			expires_at = NOW() + make_interval(secs => $4) WHERE id = $1 RETURNING expires_at`,
		c.id, issued.SessionID, contact.masked, s.ttl().Seconds()).Scan(&out.ExpiresAt); err != nil {
		return Challenge{}, fmt.Errorf("record the new code: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", c.id); err != nil {
		return Challenge{}, err
	}
	if err := tx.Commit(); err != nil {
		return Challenge{}, err
	}
	return out, s.send(ctx, userHK, c.id, Purpose(c.purpose), contact, issued.Code, c.details)
}

// resendable refuses a confirmation that has nothing to send again.
func (s Service) resendable(c stored) error {
	switch {
	case c.denied:
		return ErrDenied
	case c.confirmed:
		return ErrAlreadyConfirmed
	case c.failed >= s.maxAttempts():
		return ErrTooManyAttempts
	case Method(c.method) == MethodTOTP:
		return ErrMethodUnavailable
	}
	return nil
}

// relock locks, in tx, the confirmation that c read without a lock, and
// checks that it still has something to send by the same method.
func (s Service) relock(ctx context.Context, tx *sql.Tx, userHK string, c stored) (stored, error) {
	locked, err := s.load(ctx, tx, userHK, c.id)
	if err != nil {
		return stored{}, err
	}
	if err := s.resendable(locked); err != nil {
		return stored{}, err
	}
	if locked.method != c.method {
		return stored{}, ErrMethodUnavailable
	}
	return locked, nil
}

// restartPasskey begins a new passkey ceremony of the confirmation c, whose
// previous one an attempt took, and gives the confirmation a new lifetime.
func (s Service) restartPasskey(ctx context.Context, userHK string, c stored) (Challenge, error) {
	if s.Passkeys == nil {
		return Challenge{}, ErrMethodUnavailable
	}
	options, err := s.Passkeys.BeginAssertion(ctx, userHK, passkeyScope(c.id))
	if errors.Is(err, passkey.ErrDisabled) || errors.Is(err, passkey.ErrNoPasskey) {
		return Challenge{}, ErrMethodUnavailable
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("begin the passkey: %w", err)
	}
	out := Challenge{ID: c.id, Method: MethodPasskey, Details: c.details}
	if out.Passkey, err = optionsOf(options); err != nil {
		return Challenge{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback()
	if _, err := s.relock(ctx, tx, userHK, c); err != nil {
		return Challenge{}, err
	}
	if err := tx.QueryRowContext(ctx, `UPDATE operation_confirmations SET expires_at = NOW() + make_interval(secs => $2)
		WHERE id = $1 RETURNING expires_at`, c.id, s.ttl().Seconds()).Scan(&out.ExpiresAt); err != nil {
		return Challenge{}, fmt.Errorf("renew the confirmation: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", c.id); err != nil {
		return Challenge{}, err
	}
	return out, tx.Commit()
}

// Consume uses the confirmation id for an operation of the user, in tx,
// the transaction of the operation: the confirmation must be of purpose
// and digest, confirmed, not consumed, and confirmed no longer ago than
// the lifetime of a code. Once the transaction commits, it confirms
// nothing else. If the operation rolls back, the confirmation is not
// consumed.
func (s Service) Consume(ctx context.Context, tx *sql.Tx, userHK, id string, p Purpose, digest []byte) error {
	c, err := s.load(ctx, tx, userHK, id)
	if err != nil {
		return err
	}
	if c.purpose != string(p) || subtle.ConstantTimeCompare(c.digest, digest) != 1 {
		return ErrMismatch
	}
	switch {
	case c.denied:
		return ErrDenied
	case c.consumed:
		return ErrConsumed
	case !c.confirmed:
		return ErrNotConfirmed
	case !c.usable:
		return ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operation_confirmations SET consumed_at = NOW() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("consume the confirmation: %w", err)
	}
	return rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", id)
}

// Deny marks the confirmation id of the user denied, in tx, the
// transaction of a "this was not me" link (#374): it can no longer be
// confirmed, sent again, or consumed. A confirmation that an operation
// consumed already is marked too, for the record; stopping what the
// operation did is the caller's part. It returns what the confirmation
// was for. A confirmation that does not match its seal is not changed:
// the error wraps ErrUnverified.
func (s Service) Deny(ctx context.Context, tx *sql.Tx, userHK, id string) (Details, error) {
	c, err := s.load(ctx, tx, userHK, id)
	if err != nil {
		return Details{}, err
	}
	if c.denied {
		return c.details, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operation_confirmations SET denied_at = NOW() WHERE id = $1`, id); err != nil {
		return Details{}, fmt.Errorf("deny the confirmation: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OperationConfirmations, "id = $1", id); err != nil {
		return Details{}, err
	}
	return c.details, nil
}

// Denial is what a "this was not me" link did (#374), as its notice tells
// the user.
type Denial struct {
	// Details are what the denied confirmation was for.
	Details Details
	// Cancelled counts the withdrawals that were cancelled, and InProgress
	// those that were being paid already, which only support can follow.
	Cancelled, InProgress int
	// HoldHours is the length of the security hold that started.
	HoldHours int
}

// NotifyDenied tells the user, at the verified email address of the
// account, what a "this was not me" link did (#374), and how to reach
// support. The email shows the anti-phishing code (#375). The notice
// carries no link of its own.
func (s Service) NotifyDenied(ctx context.Context, userHK string, d Denial) error {
	if s.Sender == nil || !s.Sender.Supports(notify.ContactEmail) {
		return nil
	}
	contacts, err := s.contacts(ctx, userHK)
	if err != nil {
		return err
	}
	for _, to := range contacts {
		if to.typ != notify.ContactEmail {
			continue
		}
		msg := notify.OTPMessage{ContactType: to.typ, Contact: to.value, MaskedContact: to.masked, Purpose: notify.PurposeDenied,
			Details: d.Details.String(), Amount: d.Details.Amount, Destination: d.Details.Destination,
			Cancelled: d.Cancelled, InProgress: d.InProgress, HoldHours: d.HoldHours}
		if s.AntiPhishing != nil {
			if msg.AntiPhishingCode, err = s.AntiPhishing.ShowTo(ctx, userHK, to.typ, to.value); err != nil {
				log.Printf("Failed to read the anti-phishing code of user %s for a denial notice: %v", userHK, err)
				msg.AntiPhishingCode = ""
			}
		}
		if err := s.Sender.SendOTP(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}
