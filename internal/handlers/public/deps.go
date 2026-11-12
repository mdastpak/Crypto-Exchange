package public

import (
	"context"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/passkey"
	"github.com/mdastpak/crypto-exchange/internal/platform"
	"github.com/mdastpak/crypto-exchange/internal/signin"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
)

// The dependencies of the public routes (#48). The handler holds each one
// as an interface of what it uses, so that a test builds it with fakes;
// NewHandler and the composition root give it the stores of the database.

// Accounts finds the account of a sign-in, creates it for a new contact,
// and reads what the second step and a recovery need: *signin.Accounts.
type Accounts interface {
	FindOrCreate(ctx context.Context, origin audit.Event, contactType, contact string) (uuid.UUID, error)
	Owner(ctx context.Context, contactType, contact string) (actorType, hk string)
	CanSignIn(ctx context.Context, userHK uuid.UUID) (allowed, frozen bool, err error)
	ContactID(ctx context.Context, userHK uuid.UUID, contactType, contact string) (int64, error)
	Contact(ctx context.Context, userHK string, contactID int64) (contactType, value string, err error)
	NationalID(ctx context.Context, userHK string) (string, bool, error)
	FavoriteCurrencies(ctx context.Context, userHK string) (map[string]bool, error)
}

// AuditLog records the events of the sign-ins: audit.Log.
type AuditLog interface {
	Record(ctx context.Context, e audit.Event) error
}

// CodeStore issues and checks the sign-in codes: *otp.Store.
type CodeStore interface {
	Issue(ctx context.Context, contact, client string) (otp.Issued, error)
	Verify(ctx context.Context, sessionID, code string) (string, error)
}

// CurrencyCatalog lists the public currencies: *currencies.Catalog.
type CurrencyCatalog interface {
	List(ctx context.Context) ([]models.Currency, error)
	Get(ctx context.Context, hk string) (models.Currency, bool, error)
}

// StatusSource serves what users see of the platform controls:
// *platform.Watcher.
type StatusSource interface {
	Status() platform.Status
}

// PasswordStore checks and resets the account passwords, and runs the
// account recoveries: *userauth.Passwords.
type PasswordStore interface {
	Has(ctx context.Context, userHK string) (bool, error)
	Verify(ctx context.Context, userHK, password string) error
	MinPasswordLength() int
	CodesLeft(ctx context.Context, userHK string) (int, error)
	Reset(ctx context.Context, userHK, recoveryCode, password string, event audit.Event) (time.Time, error)
	ResetWithFactor(ctx context.Context, userHK, password string, event audit.Event) (time.Time, error)
	OpenRecovery(ctx context.Context, userHK string) (*userauth.Recovery, error)
	AdvanceRecovery(ctx context.Context, userHK string, proof userauth.RecoveryProof, plan func() (string, []string), event audit.Event) (*userauth.Recovery, error)
	CompleteRecovery(ctx context.Context, userHK string, contactID int64, password string, event audit.Event) (userauth.SetResult, error)
}

// AuthenticatorStore checks the authenticator apps: *userauth.TOTP.
type AuthenticatorStore interface {
	Enabled(ctx context.Context, userHK string) (bool, error)
	Verify(ctx context.Context, userHK, code string) error
}

// PasskeyVerifier checks the passkeys of a second step: *passkey.Service.
type PasskeyVerifier interface {
	Count(ctx context.Context, userHK string) (int, error)
	BeginAssertion(ctx context.Context, userHK, scope string) (*protocol.CredentialAssertion, error)
	FinishAssertion(ctx context.Context, userHK, scope string, response []byte) error
}

// Elevator elevates the session of a sign-in that proved the second
// factor: *userauth.Guard.
type Elevator interface {
	Elevate(ctx context.Context, userHK, sessionID string) (time.Time, error)
}

// The implementations of the server.
var (
	_ Accounts           = (*signin.Accounts)(nil)
	_ AuditLog           = audit.Log{}
	_ CodeStore          = (*otp.Store)(nil)
	_ CurrencyCatalog    = (*currencies.Catalog)(nil)
	_ StatusSource       = (*platform.Watcher)(nil)
	_ PasswordStore      = (*userauth.Passwords)(nil)
	_ AuthenticatorStore = (*userauth.TOTP)(nil)
	_ PasskeyVerifier    = (*passkey.Service)(nil)
	_ Elevator           = (*userauth.Guard)(nil)
)
