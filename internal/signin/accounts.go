// Package signin finds and creates the accounts of the user sign-ins (#48):
// the account that owns a contact, a new account for a contact that no
// account owns, the recheck that a mobile number still belongs to the
// national ID of its account (#252), and what the second step and an
// account recovery read of an account. The handlers of the public routes
// use it through an interface, and run no SQL of their own.
package signin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors of a sign-in.
var (
	// ErrAccountUnavailable reports an account that is deleted, whose
	// status does not allow signing in, or whose rows fail their seals.
	ErrAccountUnavailable = errors.New("account cannot sign in")
	// ErrAccountFrozen reports an account that a restriction froze (#220).
	ErrAccountFrozen = fmt.Errorf("%w: frozen", ErrAccountUnavailable)
	// ErrIdentityUnavailable reports a mobile number whose ownership is due
	// for a recheck that the provider cannot answer now (#252).
	ErrIdentityUnavailable = errors.New("the mobile ownership cannot be checked now")
	// ErrAdminAccount reports an administrator account at user sign-in.
	ErrAdminAccount = errors.New("administrators cannot use user sign-in")
	// errUserAlreadyExists reports that another request created a user with
	// the same contact between the lookup and the insert.
	errUserAlreadyExists = errors.New("user with this contact already exists")
)

const uniqueViolationCode = "23505"

// SignInContactTypes are the contacts that a user can sign in with.
var SignInContactTypes = map[string]bool{string(account.ContactEmail): true, string(account.ContactMobile): true}

// Accounts finds and creates the accounts of the sign-ins in DB.
type Accounts struct {
	DB *sql.DB
	// Ownership and Sealer recheck that a mobile number still belongs to
	// the national ID of its account (#252, #253). Without Ownership, a
	// recheck finds nothing to check.
	Ownership identity.MobileOwnership
	Sealer    identity.Sealer
	// Config is the configuration of the process: the recheck interval and
	// timeout of identity.mobile_ownership, and security.change_hold_hours.
	// Nil reads as the zero configuration.
	Config *config.Configuration
}

var zeroConfig config.Configuration

func (a *Accounts) settings() *config.Configuration {
	if a.Config == nil {
		return &zeroConfig
	}
	return a.Config
}

// userByContactQuery finds the user that owns a contact, through the unique
// index of user_contacts, with the sealed columns of both rows (#139).
var userByContactQuery = `SELECT u.hk, u.status, u.role, u.deleted_at IS NOT NULL, ` + restrictions.FrozenSQL("u.hk") + `,
	` + rowseal.UserContacts.Fields("c") + `, ` + rowseal.Users.Fields("u") + `
	FROM user_contacts c JOIN users u ON u.hk = c.user_hk WHERE ` + db.ContactMatch

// contactOwnerRow is a row of userByContactQuery.
type contactOwnerRow struct {
	hk              uuid.UUID
	status, role    string
	deleted, frozen bool
	contact, user   *rowseal.Row
}

// find reads the account that owns a contact.
func (o *contactOwnerRow) find(ctx context.Context, q *sql.DB, contactType, contact string) error {
	o.contact, o.user = rowseal.UserContacts.NewRow(), rowseal.Users.NewRow()
	dest := append([]any{&o.hk, &o.status, &o.role, &o.deleted, &o.frozen}, o.contact.Dest()...)
	return q.QueryRowContext(ctx, userByContactQuery, contactType, contact).Scan(append(dest, o.user.Dest()...)...)
}

// verified checks the seals of the contact and its account (#139): a
// contact moved to another account, or an account changed, outside the
// backend is refused in enforce mode.
func (o *contactOwnerRow) verified(ctx context.Context) error {
	if err := rowseal.Check(ctx, rowseal.UserContacts, o.contact); err != nil {
		return err
	}
	return rowseal.Check(ctx, rowseal.Users, o.user)
}

// Owner returns the actor type and hk of the account that owns a contact,
// for the audit event of a sign-in. Without one, or when it cannot be
// looked up, the actor is anonymous: the event is recorded either way, and
// never with the contact.
func (a *Accounts) Owner(ctx context.Context, contactType, contact string) (string, string) {
	if a.DB == nil || !SignInContactTypes[contactType] {
		return audit.ActorAnonymous, ""
	}
	var o contactOwnerRow
	if err := o.find(ctx, a.DB, contactType, contact); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Failed to look up the account of a sign-in for the audit log: %v", err)
		}
		return audit.ActorAnonymous, ""
	}
	if o.role == auth.RoleAdmin {
		return audit.ActorAdmin, o.hk.String()
	}
	return audit.ActorUser, o.hk.String()
}

// FindOrCreate returns the user that owns a contact, and creates one when
// none does. origin holds the request's fields for the audit event of a new
// user.
func (a *Accounts) FindOrCreate(ctx context.Context, origin audit.Event, contactType, contact string) (uuid.UUID, error) {
	return a.signIn(ctx, origin, contactType, contact, true)
}

// signIn is FindOrCreate. recheck says whether the ownership of a mobile
// number may still be checked: once the number was taken off an account
// that no longer owns it, the sign-in continues without it.
func (a *Accounts) signIn(ctx context.Context, origin audit.Event, contactType, contact string, recheck bool) (uuid.UUID, error) {
	if !SignInContactTypes[contactType] {
		return uuid.Nil, fmt.Errorf("unsupported contact type: %s", contactType)
	}
	var o contactOwnerRow
	find := func() error { return o.find(ctx, a.DB, contactType, contact) }

	err := find()
	if errors.Is(err, sql.ErrNoRows) {
		userHK, createErr := a.create(ctx, origin, contactType, contact)
		if !errors.Is(createErr, errUserAlreadyExists) {
			return userHK, createErr
		}
		// A concurrent sign-in for the same contact won the insert; continue
		// with that user so its status checks still apply.
		err = find()
	}
	if err != nil {
		log.Printf("Database error: %v", err)
		return uuid.Nil, fmt.Errorf("database error. please try again")
	}
	if err := o.verified(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrAccountUnavailable, err)
	}
	if o.deleted || !account.UserStatus(o.status).CanSignIn() {
		return uuid.Nil, fmt.Errorf("%w: status %s, deleted %t", ErrAccountUnavailable, o.status, o.deleted)
	}
	if o.frozen {
		return uuid.Nil, ErrAccountFrozen
	}
	if o.role == auth.RoleAdmin {
		// Administrators sign in at /admin/auth with a second factor; a
		// user token for their account is never issued.
		return uuid.Nil, ErrAdminAccount
	}
	if recheck && contactType == string(account.ContactMobile) {
		moved, err := a.recheckOwnership(ctx, origin, o.hk.String(), contact)
		if err != nil {
			return uuid.Nil, err
		}
		if moved {
			// The number no longer reaches that account: whoever holds it
			// now signs in as for an unknown number, to a new account.
			return a.signIn(ctx, origin, contactType, contact, false)
		}
	}
	return o.hk, nil
}

// recheckOwnership checks, at most once every
// identity.mobile_ownership.recheck_days, that a mobile number still
// belongs to the national ID of the account ownerHK that holds it (#252).
// A number that no longer does is taken off the account, which keeps its
// data, and recheckOwnership reports true. An account without a national
// ID is at level 0 and holds no funds, so it is not checked. When the
// provider cannot answer a check that is due, the sign-in is refused
// rather than risk showing the account to a new subscriber of the number.
func (a *Accounts) recheckOwnership(ctx context.Context, origin audit.Event, ownerHK, mobile string) (bool, error) {
	owner, found, err := contactclaim.FindOwner(ctx, a.DB, string(account.ContactMobile), mobile)
	if err != nil || !found || owner.UserHK != ownerHK {
		return false, err
	}
	cfg := a.settings()
	if owner.OwnershipCheckedAt != nil && time.Since(*owner.OwnershipCheckedAt) < cfg.Identity.MobileOwnership.Recheck() {
		return false, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, cfg.Identity.MobileOwnership.Timeout())
	defer cancel()
	verdict, ref, err := contactclaim.Check(checkCtx, a.Ownership, a.Sealer, a.DB, ownerHK, mobile)
	if errors.Is(err, identity.ErrUnavailable) {
		log.Printf("A due ownership check of a mobile number at sign-in failed: %v", err)
		return false, ErrIdentityUnavailable
	}
	if err != nil {
		return false, err
	}
	switch verdict {
	case contactclaim.NoNationalID:
		return false, nil
	case contactclaim.Owned:
		if err := contactclaim.RecordCheck(ctx, a.DB, ownerHK, mobile, a.Ownership.Provider(), ref); err != nil {
			log.Printf("Failed to record an ownership check: %v", err)
		}
		return false, nil
	}

	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := contactclaim.LockUsers(ctx, tx, ownerHK); err != nil {
		return false, err
	}
	detachOrigin := origin
	detachOrigin.ActorType, detachOrigin.ActorID = audit.ActorSystem, ""
	_, detached, err := contactclaim.Detach(ctx, tx, ownerHK, string(account.ContactMobile), mobile, contactclaim.ReasonOwnershipMismatch,
		detachOrigin, time.Duration(cfg.Security.ChangeHoldHours)*time.Hour)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// A concurrent sign-in may have taken it off first; either way, the
	// account no longer holds it.
	if !detached {
		log.Printf("A mobile number was already taken off its account by a concurrent sign-in")
	}
	return true, nil
}

// create creates an active user with its first contact, and records its
// creation, in one transaction. The contact is verified: users are created
// after a correct sign-in code, which proves control of it.
func (a *Accounts) create(ctx context.Context, origin audit.Event, contactType, contact string) (uuid.UUID, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("start the transaction: %w", err)
	}
	defer tx.Rollback()

	// A lockdown of sign-up refuses new accounts (#235). It is checked in
	// the transaction, so that a sign-up in flight when the lockdown is
	// created either commits before it or is refused.
	if err := platform.Check(ctx, tx, platform.FeatureSignUp, platform.Subject{IP: origin.IP}); err != nil {
		return uuid.Nil, err
	}

	var userHK uuid.UUID
	if err := tx.QueryRowContext(ctx, `INSERT INTO users (status) VALUES ('ACTIVE') RETURNING hk`).Scan(&userHK); err != nil {
		log.Printf("Failed to insert new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", userHK); err != nil {
		log.Printf("Failed to seal a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := db.InsertContact(ctx, tx, userHK.String(), contactType, contact, true); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
			return uuid.Nil, errUserAlreadyExists
		}
		log.Printf("Failed to insert the contact of a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	created := origin
	created.Action, created.Outcome = audit.ActionUserCreate, audit.Success
	created.ActorType, created.ActorID = audit.ActorUser, userHK.String()
	created.TargetType, created.TargetID = audit.TargetUser, userHK.String()
	if err := audit.Record(ctx, tx, created); err != nil {
		log.Printf("Failed to record the creation of a user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit a new user: %v", err)
		return uuid.Nil, fmt.Errorf("failed to create user. please try again")
	}
	return userHK, nil
}

// CanSignIn reads whether an account may still sign in: it may have been
// disabled or frozen since its code was verified. An account that does not
// exist may not.
func (a *Accounts) CanSignIn(ctx context.Context, userHK uuid.UUID) (allowed, frozen bool, err error) {
	var status, role string
	var deleted bool
	err = a.DB.QueryRowContext(ctx, `SELECT status, role, deleted_at IS NOT NULL, `+restrictions.FrozenSQL("users.hk")+`
		FROM users WHERE hk = $1`, userHK).Scan(&status, &role, &deleted, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return !deleted && role == auth.RoleUser && account.UserStatus(status).CanSignIn(), frozen, nil
}

// ContactID returns the id of the verified contact of userHK that a sign-in
// code went to, or 0 when the account has no such contact any more.
func (a *Accounts) ContactID(ctx context.Context, userHK uuid.UUID, contactType, contact string) (int64, error) {
	var id int64
	err := a.DB.QueryRowContext(ctx, `SELECT c.id FROM user_contacts c
		WHERE c.user_hk = $3 AND c.verified_at IS NOT NULL AND `+db.ContactMatch, contactType, contact, userHK).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Contact returns the type and value of a contact of userHK that is not
// removed, or sql.ErrNoRows.
func (a *Accounts) Contact(ctx context.Context, userHK string, contactID int64) (contactType, value string, err error) {
	err = a.DB.QueryRowContext(ctx, `SELECT type, value FROM user_contacts
		WHERE id = $1 AND user_hk = $2 AND removed_at IS NULL`, contactID, userHK).Scan(&contactType, &value)
	return contactType, value, err
}

// NationalID returns the verified national ID of userHK, opened with
// Sealer, and whether it has one.
func (a *Accounts) NationalID(ctx context.Context, userHK string) (string, bool, error) {
	return identity.NationalID(ctx, a.DB, a.Sealer, userHK)
}

// FavoriteCurrencies returns the currencies that userHK keeps as favorites.
func (a *Accounts) FavoriteCurrencies(ctx context.Context, userHK string) (map[string]bool, error) {
	return favorites.CurrencyHKs(ctx, a.DB, userHK)
}
