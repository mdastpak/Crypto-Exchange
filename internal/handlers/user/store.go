package user

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBStore is the database of the user routes (#48): the contacts, their
// changes, the national ID, and the profile. The handler uses it through
// Store, so that a test can give it a fake; the SQL of the routes is here,
// not in the handler.
type DBStore struct {
	DB *sql.DB
	// Sealer opens the national IDs. Without it, a user who has one cannot
	// change their mobile number.
	Sealer identity.Sealer
	// Ownership checks whether a mobile number held by another account
	// still belongs to its national ID, before a claim (#252).
	Ownership identity.MobileOwnership
	// Config is the configuration of the process: the cooldown of new
	// contacts, the security hold, and the timeout of the ownership checks.
	// Nil reads as the zero configuration.
	Config *config.Configuration
}

func (s *DBStore) settings() *config.Configuration {
	if s.Config == nil {
		return &zeroConfig
	}
	return s.Config
}

// Contacts returns the verified contacts of a user; see verifiedContacts.
func (s *DBStore) Contacts(ctx context.Context, userHK string) ([]storedContact, time.Time, error) {
	return verifiedContacts(ctx, s.DB, userHK)
}

// NationalID returns the verified national ID of a user, and maps keys that
// are not loaded to errIdentityUnavailable.
func (s *DBStore) NationalID(ctx context.Context, userHK string) (string, bool, error) {
	return s.nationalID(ctx, s.DB, userHK)
}

func (s *DBStore) nationalID(ctx context.Context, q identity.Querier, userHK string) (string, bool, error) {
	id, has, err := identity.NationalID(ctx, q, s.Sealer, userHK)
	if errors.Is(err, identity.ErrNoKeys) {
		log.Printf("A user has a national ID, but the data-protection keys are not loaded")
		return "", false, errIdentityUnavailable
	}
	return id, has, err
}

// ContactOwner returns the account that holds a contact.
func (s *DBStore) ContactOwner(ctx context.Context, contactType, value string) (contactclaim.Owner, bool, error) {
	return contactclaim.FindOwner(ctx, s.DB, contactType, value)
}

// ClaimCheck checks whether the mobile number of ownerHK still belongs to
// the national ID of that account, within the timeout of
// identity.mobile_ownership; see contactclaim.Check.
func (s *DBStore) ClaimCheck(ctx context.Context, ownerHK, mobile string) (contactclaim.Verdict, error) {
	ctx, cancel := context.WithTimeout(ctx, s.settings().Identity.MobileOwnership.Timeout())
	defer cancel()
	verdict, _, err := contactclaim.Check(ctx, s.Ownership, s.Sealer, s.DB, ownerHK, mobile)
	return verdict, err
}

// AccountName returns a contact of a user, under which an authenticator or
// a passkey shows the account, or "account".
func (s *DBStore) AccountName(ctx context.Context, userHK string) string {
	var id int64
	var plain *string
	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT id, value, value_enc FROM user_contacts
		WHERE user_hk = $1 AND removed_at IS NULL AND type IN ('email', 'mobile')
		ORDER BY is_primary DESC, id LIMIT 1`, userHK).Scan(&id, &plain, &sealed); err != nil {
		return "account"
	}
	name, err := db.OpenContact(id, plain, sealed)
	if err != nil {
		log.Printf("Failed to open contact %d for the name of an account: %v", id, err)
		return "account"
	}
	return name
}

// Profile returns the profile of a user; see loadProfile.
func (s *DBStore) Profile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return loadProfile(ctx, s.DB, userID, false)
}

// UpdateProfile applies edit to the profile of a user and returns it. The
// row is locked from the read to the write, so that the account checks hold
// for the write, and concurrent updates of different fields keep each
// other's changes. A change commits only with its audit event from origin
// (#195).
func (s *DBStore) UpdateProfile(ctx context.Context, userID uuid.UUID, edit func(*Profile), origin audit.Event) (Profile, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback()
	profile, err := loadProfile(ctx, tx, userID, true)
	if err != nil {
		return Profile{}, err
	}
	before := profile
	edit(&profile)
	// The names are written in plaintext and sealed (#140); the release
	// after this one writes them sealed only.
	sealed, err := db.SealNames(userID.String(), db.Names{First: profile.FirstName, Last: profile.LastName, Display: profile.DisplayName})
	if err != nil {
		return Profile{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET first_name = $1, last_name = $2, display_name = $3, locale = $4, theme = $5, updated_at = NOW(),
			first_name_enc = $7, last_name_enc = $8, display_name_enc = $9
		WHERE hk = $6`,
		profile.FirstName, profile.LastName, profile.DisplayName, profile.Locale, profile.Theme, userID,
		sealed.First, sealed.Last, sealed.Display,
	); err != nil {
		return Profile{}, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", userID); err != nil {
		return Profile{}, err
	}
	if changes := profileChanges(before, profile); changes != nil {
		e := audit.Of(origin, audit.ActionUserProfileUpdate, audit.TargetUser, userID.String())
		e.ActorType, e.ActorID = audit.ActorUser, userID.String()
		e.Changes = changes
		if err := audit.Record(ctx, tx, e); err != nil {
			return Profile{}, err
		}
	}
	return profile, tx.Commit()
}

// ApplyChange applies a proven change of a contact in one transaction,
// under the lock of the user's row, after checking it again against the
// contacts as they are now. event is its audit event, from the request. It
// returns the contacts after the change.
func (s *DBStore) ApplyChange(ctx context.Context, event audit.Event, ch pendingChange, owned *ownership, claimFrom string) ([]storedContact, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
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
	plan, err := planChange(cooldownOf(s.settings()), ContactChangeRequest{Action: ch.Action, Type: ch.Type, Value: ch.Value, VerifyWith: accountType(contacts, ch.AccountContact)}, contacts, now)
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
		nationalID, has, err := s.nationalID(ctx, tx, ch.UserID)
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

	e := event
	e.Outcome = audit.Success
	hold := time.Duration(s.settings().Security.ChangeHoldHours) * time.Hour

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
			origin := event
			origin.ActorType, origin.ActorID = audit.ActorUser, ch.UserID
			if _, claimed, err = contactclaim.Detach(ctx, tx, claimFrom, plan.typ, plan.value, contactclaim.ReasonClaimed, origin, hold); err != nil {
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
		var id int64
		err = tx.QueryRowContext(ctx, `UPDATE user_contacts c SET value = $3, verified_at = NOW(), updated_at = NOW(),
				ownership_checked_at = CASE WHEN $4::text IS NULL THEN NULL ELSE NOW() END, ownership_provider = $4, ownership_ref = $5
			WHERE user_hk = $1 AND type = $2::contact_type AND is_primary AND c.removed_at IS NULL
			RETURNING id`, ch.UserID, plan.typ, plan.value, checkedProvider, checkedRef).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The plan read the contact under the lock of the account, so
			// this does not happen; it changes nothing, as before.
			err = nil
		case err == nil:
			err = db.EncryptContact(ctx, tx, id, plan.typ, plan.value)
		}
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
	until, err := restrictions.StartSecurityHold(ctx, tx, ch.UserID, hold)
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

// verifiedContacts returns the verified email and mobile contacts of a user,
// email first, and the time of the database when it read them. The rules
// that depend on the age of a contact compare with that time, never with
// the clock of the instance, so that every instance decides the same way.
// It is the zero time when the user has no verified contact.
func verifiedContacts(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}, userHK string) ([]storedContact, time.Time, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, type::text, value, value_enc, verified_at, NOW() FROM user_contacts c
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
		var id int64
		var plain *string
		var sealed []byte
		if err := rows.Scan(&id, &ct.typ, &plain, &sealed, &ct.verifiedAt, &now); err != nil {
			return nil, time.Time{}, err
		}
		if ct.value, err = db.OpenContact(id, plain, sealed); err != nil {
			return nil, time.Time{}, err
		}
		out = append(out, ct)
	}
	return out, now, rows.Err()
}

// queryRower is implemented by *sql.DB and *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// profileQuery reads a profile.
// The contacts come as one JSON object of the primary contact of each
// type: its id, its plaintext, and its ciphertext in hex (#140).
const profileQuery = `SELECT hk, status, role,
	COALESCE((SELECT jsonb_object_agg(c.type, jsonb_build_array(c.id::text, c.value, encode(c.value_enc, 'hex')))
		FROM user_contacts c WHERE c.user_hk = users.hk AND c.is_primary AND c.removed_at IS NULL), '{}'),
	first_name, last_name, display_name, first_name_enc, last_name_enc, display_name_enc, locale, theme, kyc_status, kyc_tier,
	created_at
	FROM users WHERE hk = $1 AND deleted_at IS NULL`

// loadProfile reads the profile of a user. It returns errProfileNotFound for
// a missing or deleted user, and errAccountUnavailable for an account that
// cannot sign in or is not a user account, so that a token issued before
// such a change no longer reaches the profile.
func loadProfile(ctx context.Context, q queryRower, userID uuid.UUID, lock bool) (Profile, error) {
	query := profileQuery
	if lock {
		query += " FOR UPDATE"
	}
	var p Profile
	var role string
	var contact []byte
	var plain db.Names
	var sealed db.SealedNames
	err := q.QueryRowContext(ctx, query, userID).Scan(&p.UUID, &p.Status, &role, &contact,
		&plain.First, &plain.Last, &plain.Display, &sealed.First, &sealed.Last, &sealed.Display,
		&p.Locale, &p.Theme, &p.KYCStatus, &p.KYCTier, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, errProfileNotFound
	}
	if err != nil {
		return p, err
	}
	if !account.UserStatus(p.Status).CanSignIn() || role != auth.RoleUser {
		return p, errAccountUnavailable
	}
	names, err := db.OpenNames(p.UUID, plain, sealed)
	if err != nil {
		return p, err
	}
	p.FirstName, p.LastName, p.DisplayName = names.First, names.Last, names.Display

	var contacts map[string][]*string
	if err := json.Unmarshal(contact, &contacts); err != nil {
		return p, fmt.Errorf("decoding the contacts: %w", err)
	}
	p.Contact = make(map[string]string)
	for _, contactType := range []string{"email", "mobile"} {
		stored, ok := contacts[contactType]
		if !ok {
			continue
		}
		value, err := openStoredContact(stored)
		if err != nil {
			return p, err
		}
		p.Contact[contactType] = maskContact(contactType, value)
	}
	if p.CreatedAt != nil {
		created := p.CreatedAt.UTC()
		p.CreatedAt = &created
	}
	return p, nil
}

// openStoredContact returns the value of a contact of profileQuery: the
// array of its id, its plaintext, and its ciphertext in hex.
func openStoredContact(stored []*string) (string, error) {
	if len(stored) != 3 || stored[0] == nil {
		return "", errors.New("decoding the contacts: a contact is not [id, value, value_enc]")
	}
	id, err := strconv.ParseInt(*stored[0], 10, 64)
	if err != nil {
		return "", fmt.Errorf("decoding the contacts: %w", err)
	}
	var sealed []byte
	if stored[2] != nil {
		if sealed, err = hex.DecodeString(*stored[2]); err != nil {
			return "", fmt.Errorf("decoding contact %d: %w", id, err)
		}
	}
	return db.OpenContact(id, stored[1], sealed)
}
