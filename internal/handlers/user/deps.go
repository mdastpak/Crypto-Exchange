package user

import (
	"context"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/confirm"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/userauth"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
)

// The dependencies of the user routes (#48). The handler holds each one as
// an interface of what it uses, so that a test builds it with fakes;
// NewHandler and the composition root give it the stores of the database.

// Store is the database of the routes: the contacts and their changes, the
// national ID, and the profile: *DBStore.
type Store interface {
	Contacts(ctx context.Context, userHK string) ([]storedContact, time.Time, error)
	NationalID(ctx context.Context, userHK string) (string, bool, error)
	ContactOwner(ctx context.Context, contactType, value string) (contactclaim.Owner, bool, error)
	ClaimCheck(ctx context.Context, ownerHK, mobile string) (contactclaim.Verdict, error)
	ApplyChange(ctx context.Context, event audit.Event, ch pendingChange, owned *ownership, claimFrom string) ([]storedContact, error)
	AccountName(ctx context.Context, userHK string) string
	Profile(ctx context.Context, userID uuid.UUID) (Profile, error)
	UpdateProfile(ctx context.Context, userID uuid.UUID, edit func(*Profile), origin audit.Event) (Profile, error)
}

// AuditLog records the events that change nothing: audit.Log.
type AuditLog interface {
	Record(ctx context.Context, e audit.Event) error
}

// FavoriteStore keeps the favorites of the users: favorites.Store.
type FavoriteStore interface {
	Get(ctx context.Context, userHK string) (favorites.List, error)
	Add(ctx context.Context, userHK string, kind favorites.Kind, target string, max int) (bool, error)
	Remove(ctx context.Context, userHK string, kind favorites.Kind, target string) (bool, error)
	Reorder(ctx context.Context, userHK string, kind favorites.Kind, ordered []string) error
}

// RestrictionReader reads the restrictions and the security hold of a
// user: restrictions.Store.
type RestrictionReader interface {
	Active(ctx context.Context, userHK string) ([]restrictions.Restriction, error)
	SecurityHold(ctx context.Context, userHK string) (*time.Time, error)
}

// VerificationReader reads the verification level of a user, what it
// allows, and their withdrawal limits (#157): verification.Store.
type VerificationReader interface {
	Summary(ctx context.Context, userHK string) (verification.Summary, error)
}

// DestinationStore keeps the withdrawal destinations of users (#160):
// destinations.Store.
type DestinationStore interface {
	List(ctx context.Context, userHK string) ([]destinations.Destination, error)
	Add(ctx context.Context, userHK string, r destinations.Request, origin audit.Event) (destinations.Destination, error)
	Remove(ctx context.Context, userHK, id string, origin audit.Event) error
}

// ConfirmationService confirms operations with the second factor (#161):
// confirm.Service.
type ConfirmationService interface {
	Settings(ctx context.Context, userHK string) (confirm.Settings, error)
	SetMethod(ctx context.Context, userHK string, m confirm.Method, origin audit.Event) error
	Confirm(ctx context.Context, userHK, id, code string, origin audit.Event) error
	Resend(ctx context.Context, userHK, id, client string) (confirm.Challenge, error)
}

// CodeStore issues and checks the codes of contact changes: *otp.Store.
type CodeStore interface {
	Issue(ctx context.Context, contact, client string) (otp.Issued, error)
	Verify(ctx context.Context, sessionID, code string) (string, error)
	TTL() time.Duration
}

// PasswordStore keeps the account passwords and the recovery codes:
// *userauth.Passwords.
type PasswordStore interface {
	Has(ctx context.Context, userHK string) (bool, error)
	Status(ctx context.Context, userHK string) (userauth.Status, error)
	Set(ctx context.Context, userHK, current, password string, event audit.Event) (userauth.SetResult, error)
	Verify(ctx context.Context, userHK, password string) error
	MinPasswordLength() int
	CodesLeft(ctx context.Context, userHK string) (int, error)
	GenerateRecoveryCodes(ctx context.Context, userHK string, event audit.Event) ([]string, error)
	OpenRecovery(ctx context.Context, userHK string) (*userauth.Recovery, error)
}

// AuthenticatorStore keeps the authenticator apps: *userauth.TOTP.
type AuthenticatorStore interface {
	Enabled(ctx context.Context, userHK string) (bool, error)
	Begin(ctx context.Context, userHK, account string) (userauth.TOTPEnrollment, error)
	Confirm(ctx context.Context, userHK, code string, event audit.Event) (time.Time, error)
	Verify(ctx context.Context, userHK, code string) error
	Remove(ctx context.Context, userHK string, event audit.Event) (time.Time, error)
}

// PasskeyStore keeps the passkeys: *passkey.Service.
type PasskeyStore interface {
	Enabled() bool
	Count(ctx context.Context, userHK string) (int, error)
	List(ctx context.Context, userHK string) ([]passkey.Passkey, error)
	BeginRegistration(ctx context.Context, userHK, account string) (*protocol.CredentialCreation, error)
	FinishRegistration(ctx context.Context, userHK, name string, response []byte, event audit.Event) (passkey.Passkey, time.Time, error)
	BeginAssertion(ctx context.Context, userHK, scope string) (*protocol.CredentialAssertion, error)
	FinishAssertion(ctx context.Context, userHK, scope string, response []byte) error
	Remove(ctx context.Context, userHK, id string, event audit.Event) (time.Time, error)
}

// Guard decides whether a session may run an operation that the second
// factor protects, and elevates it: *userauth.Guard.
type Guard interface {
	Allows(ctx context.Context, userHK, sessionID string, funds bool) error
	StateOf(ctx context.Context, userHK, sessionID string) (userauth.State, error)
	Elevate(ctx context.Context, userHK, sessionID string) (time.Time, error)
}

// The implementations of the server.
var (
	_ Store               = (*DBStore)(nil)
	_ AuditLog            = audit.Log{}
	_ FavoriteStore       = favorites.Store{}
	_ RestrictionReader   = restrictions.Store{}
	_ VerificationReader  = verification.Store{}
	_ DestinationStore    = destinations.Store{}
	_ ConfirmationService = confirm.Service{}
	_ CodeStore           = (*otp.Store)(nil)
	_ PasswordStore       = (*userauth.Passwords)(nil)
	_ AuthenticatorStore  = (*userauth.TOTP)(nil)
	_ PasskeyStore        = (*passkey.Service)(nil)
	_ Guard               = (*userauth.Guard)(nil)
)
