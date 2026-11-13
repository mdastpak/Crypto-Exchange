package user

import (
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/redis/go-redis/v9"
)

// UserHandler serves the routes of the signed-in user. Every dependency is
// an interface (deps.go, #48); a nil one makes its routes unavailable.
type UserHandler struct {
	// Config is the configuration of the process (#48). NewHandler sets it;
	// nil reads as the zero configuration.
	Config *config.Configuration
	// Locales are the supported locales, which a user may save in their
	// profile (#286). Without them, no locale can be saved.
	Locales *i18n.Registry
	// Store holds the contacts, their changes, and the profile.
	Store Store
	// Audit records the events that change nothing.
	Audit        AuditLog
	Favorites    FavoriteStore
	Restrictions RestrictionReader
	// Verification reads the verification level of the user (#157).
	Verification VerificationReader
	// Destinations keeps the withdrawal destinations of the user (#160).
	// The server sets the provider that checks the owner of a card, an
	// IBAN, or an account, and the keys that read the national IDs.
	Destinations DestinationStore
	// Redis holds the pending contact changes, which any instance confirms.
	Redis redis.UniversalClient
	// Sessions lists and ends the sessions of the signed-in user (#193).
	Sessions auth.SessionManager
	// ContactOTP issues and checks the codes of contact changes (#199), in
	// their own namespace, so that they cannot sign in.
	ContactOTP CodeStore
	// OTPSender delivers those codes. The server sets the configured
	// delivery; without one, the contact changes are unavailable.
	OTPSender notify.OTPSender
	// Ownership checks that a mobile number belongs to the user's
	// national ID (#253). Without it, a user who has a verified national
	// ID cannot add or replace a mobile number.
	Ownership identity.MobileOwnership
	// Passwords keeps the account passwords, the second step of a sign-in
	// (#307). The server sets it.
	Passwords PasswordStore
	// TOTP keeps the authenticator apps, a second step that a user can
	// prove instead of the password.
	TOTP AuthenticatorStore
	// Passkeys keeps the passkeys, another second step that a user can
	// prove instead of the password.
	Passkeys PasskeyStore
	// Guard decides whether a session may run the operations that the
	// second factor protects. The server sets it.
	Guard Guard
}

// NewHandler returns the handler with the stores of the services in is. A
// service that is not there leaves its dependencies nil, never a typed nil.
func NewHandler(is *models.InfraServices) *UserHandler {
	h := &UserHandler{Config: is.Settings()}
	if is.Postgres != nil {
		h.Store = &DBStore{DB: is.Postgres.DB, Config: is.Settings()}
		h.Audit = audit.Log{DB: is.Postgres.DB}
		h.Favorites = favorites.Store{DB: is.Postgres.DB}
		h.Restrictions = restrictions.Store{DB: is.Postgres.DB}
		if policy, err := verification.NewPolicy(is.Settings().Verification); err != nil {
			log.Printf("The verification levels are unavailable: %v", err)
		} else {
			h.Verification = verification.Store{DB: is.Postgres.DB, Policy: policy}
			h.Destinations = destinations.Store{DB: is.Postgres.DB, Settings: is.Settings().Withdrawals, Levels: verification.Store{DB: is.Postgres.DB, Policy: policy}}
		}
	}
	if is.Redis != nil {
		h.Redis = is.Redis.Client
		settings := otp.SettingsOf(is.Settings())
		settings.Namespace = ContactOTPNamespace
		h.ContactOTP = otp.NewStore(is.Redis.Client, settings)
	}
	return h
}

// zeroConfig is the configuration of a handler without one.
var zeroConfig config.Configuration

// settings returns the configuration of the handler, or the zero
// configuration without one.
func (h *UserHandler) settings() *config.Configuration {
	if h.Config == nil {
		return &zeroConfig
	}
	return h.Config
}

// cooldownOf is contacts.new_contact_cooldown_hours of cfg (#250).
func cooldownOf(cfg *config.Configuration) time.Duration {
	return time.Duration(cfg.Contacts.NewContactCooldownHours) * time.Hour
}
