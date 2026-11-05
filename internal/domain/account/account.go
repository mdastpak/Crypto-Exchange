package account

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// User is a platform user or administrator: a row of the users table, with
// its contacts from user_contacts. It is the only Go type for users.
type User struct {
	// ID is users.hk, the ID that every other table references.
	ID       shared.UUID `json:"id" db:"hk"`
	Role     string      `json:"role" db:"role"` // "user" or "admin"
	Contacts []Contact   `json:"contacts"`

	// Authentication. Users sign in with a one-time code sent to a contact;
	// administrators also need an authenticator code.
	TotpSecret   string `json:"-" db:"totp_secret,omitempty"` // Encrypted TOTP secret
	TotpEnabled  bool   `json:"totp_enabled" db:"totp_enabled"`
	TotpLastStep *int64 `json:"-" db:"totp_last_step"` // The last accepted TOTP time step

	// Profile
	FirstName   string `json:"first_name,omitempty" db:"first_name"`
	LastName    string `json:"last_name,omitempty" db:"last_name"`
	DisplayName string `json:"display_name,omitempty" db:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty" db:"avatar_url"`

	// Status
	Status      UserStatus        `json:"status" db:"status"`
	LastLoginAt *shared.Timestamp `json:"last_login_at,omitempty" db:"last_login_at"`
	LastLoginIP string            `json:"last_login_ip,omitempty" db:"last_login_ip"`

	// Referral
	ReferralCode string       `json:"referral_code" db:"referral_code"`
	ReferredBy   *shared.UUID `json:"referred_by,omitempty" db:"referred_by"`

	// Compliance
	KYCStatus shared.KYCStatus `json:"kyc_status" db:"kyc_status"`
	KYCTier   shared.KYCTier   `json:"kyc_tier" db:"kyc_tier"`
	RiskScore int              `json:"risk_score" db:"risk_score"` // 0-100

	// Timestamps
	CreatedAt shared.Timestamp  `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp  `json:"updated_at" db:"updated_at"`
	DeletedAt *shared.Timestamp `json:"deleted_at,omitempty" db:"deleted_at"`
}

type UserStatus string

const (
	UserStatusActive    UserStatus = "ACTIVE"
	UserStatusInactive  UserStatus = "INACTIVE"
	UserStatusSuspended UserStatus = "SUSPENDED"
	UserStatusBanned    UserStatus = "BANNED"
	UserStatusPending   UserStatus = "PENDING_VERIFICATION"
)

func (u UserStatus) String() string {
	return string(u)
}

// CanSignIn reports whether a user with this status may sign in. A user
// whose contact is not verified yet may, since signing in verifies it.
func (u UserStatus) CanSignIn() bool {
	return u == UserStatusActive || u == UserStatusPending
}

// Contact is a way to reach a user, and to sign in with: an email address or
// a mobile number. Usernames of older versions are kept, but cannot sign in.
type Contact struct {
	Type ContactType `json:"type" db:"type"`
	// Value is the email address as entered, or the mobile number in E.164.
	Value      string            `json:"value" db:"value"`
	VerifiedAt *shared.Timestamp `json:"verified_at,omitempty" db:"verified_at"`
	IsPrimary  bool              `json:"is_primary" db:"is_primary"`
}

// ContactType is the type of a contact, as the contact_type enum names it.
type ContactType string

const (
	ContactEmail    ContactType = "email"
	ContactMobile   ContactType = "mobile"
	ContactUsername ContactType = "username"
)

// PrimaryContact returns the user's primary contact of a type.
func (u *User) PrimaryContact(t ContactType) (Contact, bool) {
	for _, c := range u.Contacts {
		if c.Type == t && c.IsPrimary {
			return c, true
		}
	}
	return Contact{}, false
}

func (u *User) IsActive() bool {
	return u.Status == UserStatusActive
}

func (u *User) CanTrade() bool {
	return u.IsActive() && u.KYCStatus == shared.KYCStatusApproved
}

// SubAccount represents a sub-account under a user (for API trading, institutional)
type SubAccount struct {
	ID          shared.UUID `json:"id" db:"id"`
	UserID      shared.UUID `json:"user_id" db:"user_id"`
	Label       string      `json:"label" db:"label"` // Human-readable name
	Description string      `json:"description,omitempty" db:"description"`

	// Permissions
	Permissions []Permission `json:"permissions" db:"permissions"` // JSON array

	// Trading limits
	DailyVolumeLimit     shared.Decimal `json:"daily_volume_limit,omitempty" db:"daily_volume_limit"`
	DailyWithdrawalLimit shared.Decimal `json:"daily_withdrawal_limit,omitempty" db:"daily_withdrawal_limit"`
	PositionLimit        shared.Decimal `json:"position_limit,omitempty" db:"position_limit"`

	// API Keys (stored separately for security)
	APIKeys []*APIKey `json:"api_keys,omitempty" db:"-"`

	// Status
	Status SubAccountStatus `json:"status" db:"status"`

	// Timestamps
	CreatedAt shared.Timestamp  `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp  `json:"updated_at" db:"updated_at"`
	DeletedAt *shared.Timestamp `json:"deleted_at,omitempty" db:"deleted_at"`
}

type SubAccountStatus string

const (
	SubAccountStatusActive   SubAccountStatus = "ACTIVE"
	SubAccountStatusInactive SubAccountStatus = "INACTIVE"
	SubAccountStatusFrozen   SubAccountStatus = "FROZEN"
)

func (s SubAccountStatus) String() string {
	return string(s)
}

func (s *SubAccount) IsActive() bool {
	return s.Status == SubAccountStatusActive
}

func (s *SubAccount) HasPermission(perm Permission) bool {
	for _, p := range s.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// Permission represents an API permission scope
type Permission string

const (
	// Read permissions
	PermissionReadAccount    Permission = "READ_ACCOUNT"
	PermissionReadBalances   Permission = "READ_BALANCES"
	PermissionReadOrders     Permission = "READ_ORDERS"
	PermissionReadTrades     Permission = "READ_TRADES"
	PermissionReadPositions  Permission = "READ_POSITIONS"
	PermissionReadMarketData Permission = "READ_MARKET_DATA"

	// Write permissions
	PermissionPlaceOrder  Permission = "PLACE_ORDER"
	PermissionCancelOrder Permission = "CANCEL_ORDER"
	PermissionModifyOrder Permission = "MODIFY_ORDER"

	// Withdrawal/transfer
	PermissionWithdraw Permission = "WITHDRAW"
	PermissionTransfer Permission = "TRANSFER"

	// Admin
	PermissionAdmin Permission = "ADMIN"
)

func (p Permission) String() string {
	return string(p)
}

// APIKey represents an API key for programmatic access
type APIKey struct {
	ID           shared.UUID `json:"id" db:"id"`
	SubAccountID shared.UUID `json:"sub_account_id" db:"sub_account_id"`

	// Key info
	Name      string     `json:"name" db:"name"`             // User-defined label
	PublicKey string     `json:"public_key" db:"public_key"` // Key ID, random and independent of the secret
	KeyType   APIKeyType `json:"key_type" db:"key_type"`     // HMAC, ED25519

	// Permissions (subset of sub-account permissions)
	Permissions []Permission `json:"permissions" db:"permissions"`

	// IP whitelist (CIDR notation)
	IPWhitelist []string `json:"ip_whitelist,omitempty" db:"ip_whitelist"`

	// Rate limits (per second)
	RateLimitREST  int `json:"rate_limit_rest" db:"rate_limit_rest"`   // Requests/sec
	RateLimitWS    int `json:"rate_limit_ws" db:"rate_limit_ws"`       // Messages/sec
	RateLimitTrade int `json:"rate_limit_trade" db:"rate_limit_trade"` // Orders/sec

	// Status
	Status     APIKeyStatus      `json:"status" db:"status"`
	LastUsedAt *shared.Timestamp `json:"last_used_at,omitempty" db:"last_used_at"`
	LastUsedIP string            `json:"last_used_ip,omitempty" db:"last_used_ip"`
	ExpiresAt  *shared.Timestamp `json:"expires_at,omitempty" db:"expires_at"`

	// Verification material. An HMAC key keeps its secret sealed with a
	// field-enc key (SealAPIKeySecret); an Ed25519 key keeps only the
	// client's public key. Neither is ever returned by the API.
	SealedSecret     []byte `json:"-" db:"secret_sealed"`
	Ed25519PublicKey []byte `json:"-" db:"ed25519_public_key"`

	CreatedAt shared.Timestamp  `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp  `json:"updated_at" db:"updated_at"`
	RevokedAt *shared.Timestamp `json:"revoked_at,omitempty" db:"revoked_at"`
}

type APIKeyType string

const (
	APIKeyTypeHMAC    APIKeyType = "HMAC"    // HMAC-SHA256
	APIKeyTypeEd25519 APIKeyType = "ED25519" // Ed25519 signing
)

func (a APIKeyType) String() string {
	return string(a)
}

type APIKeyStatus string

const (
	APIKeyStatusActive   APIKeyStatus = "ACTIVE"
	APIKeyStatusInactive APIKeyStatus = "INACTIVE"
	APIKeyStatusRevoked  APIKeyStatus = "REVOKED"
	APIKeyStatusExpired  APIKeyStatus = "EXPIRED"
)

func (a APIKeyStatus) String() string {
	return string(a)
}

// apiKeyIDBytes is the random part of a key ID: 128 bits, so that key IDs
// never collide and cannot be guessed.
const apiKeyIDBytes = 16

// GeneratedAPIKey is a new API key. Secret is shown to the client once and
// never stored in plaintext.
type GeneratedAPIKey struct {
	// KeyID is the public identifier, random and independent of the secret.
	KeyID string
	Type  APIKeyType
	// Secret is, for HMAC, the shared secret, and for Ed25519, the 32-byte
	// seed of the private key, both base64url-encoded without padding.
	Secret string
	// PublicKey is the Ed25519 public key, which the server keeps to verify
	// signatures. It is nil for HMAC, whose secret the server keeps sealed
	// (SealAPIKeySecret).
	PublicKey ed25519.PublicKey
}

// NewAPIKeyID returns a random key ID for keyType, such as
// "hmac_3f2a...". Clients that upload their own Ed25519 public key get one
// as well.
func NewAPIKeyID(keyType APIKeyType) (string, error) {
	prefix := map[APIKeyType]string{APIKeyTypeHMAC: "hmac_", APIKeyTypeEd25519: "ed25519_"}[keyType]
	if prefix == "" {
		return "", errors.New("unsupported key type")
	}
	random := make([]byte, apiKeyIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random), nil
}

// GenerateAPIKey creates a new API key: a random key ID and, for HMAC, a
// random 256-bit secret, or, for Ed25519, a key pair.
func GenerateAPIKey(keyType APIKeyType) (GeneratedAPIKey, error) {
	keyID, err := NewAPIKeyID(keyType)
	if err != nil {
		return GeneratedAPIKey{}, err
	}
	key := GeneratedAPIKey{KeyID: keyID, Type: keyType}
	switch keyType {
	case APIKeyTypeHMAC:
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return GeneratedAPIKey{}, err
		}
		key.Secret = base64.RawURLEncoding.EncodeToString(secret)
	case APIKeyTypeEd25519:
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return GeneratedAPIKey{}, err
		}
		key.Secret = base64.RawURLEncoding.EncodeToString(private.Seed())
		key.PublicKey = public
	}
	return key, nil
}

// ParseEd25519PublicKey decodes a public key that a client uploads:
// 32 bytes, base64-encoded in the standard or URL alphabet, with or
// without padding.
func ParseEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	encoded = strings.TrimRight(strings.TrimSpace(encoded), "=")
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if raw, err := encoding.DecodeString(encoded); err == nil && len(raw) == ed25519.PublicKeySize {
			return ed25519.PublicKey(raw), nil
		}
	}
	return nil, fmt.Errorf("an Ed25519 public key is %d bytes, base64-encoded", ed25519.PublicKeySize)
}

// VerifyHMACSignature reports whether signature is the HMAC-SHA-256 of
// message under secret. It compares in constant time, so the time it takes
// reveals nothing about the expected signature.
func VerifyHMACSignature(secret, message, signature []byte) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(message)
	return hmac.Equal(mac.Sum(nil), signature)
}

// VerifyEd25519Signature reports whether signature is a valid Ed25519
// signature of message by publicKey.
func VerifyEd25519Signature(publicKey ed25519.PublicKey, message, signature []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(publicKey, message, signature)
}

// FieldSealer encrypts values bound to their place in the database.
// *keys.Keyring implements it with the field-enc keys.
type FieldSealer interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
}

// apiKeySecretAAD binds a sealed secret to its API key, so that it cannot
// be copied to the row of another key.
func apiKeySecretAAD(keyID string) []byte {
	return []byte("api_keys.secret_sealed:" + keyID)
}

// SealAPIKeySecret encrypts the secret of an HMAC key for its row in
// api_keys (secret_sealed).
func SealAPIKeySecret(sealer FieldSealer, keyID string, secret []byte) ([]byte, error) {
	return sealer.SealField(apiKeySecretAAD(keyID), secret)
}

// OpenAPIKeySecret decrypts the secret of the HMAC key keyID, which the
// server needs to verify the key's signatures.
func OpenAPIKeySecret(sealer FieldSealer, keyID string, sealed []byte) ([]byte, error) {
	return sealer.OpenField(apiKeySecretAAD(keyID), sealed)
}

// Session represents an authenticated session
type Session struct {
	ID           shared.UUID  `json:"id" db:"id"`
	UserID       shared.UUID  `json:"user_id" db:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty" db:"sub_account_id"`

	// Auth method
	AuthMethod AuthMethod `json:"auth_method" db:"auth_method"`

	// Token info
	AccessTokenHash  string `json:"-" db:"access_token_hash"`
	RefreshTokenHash string `json:"-" db:"refresh_token_hash"`

	// Client info
	UserAgent string `json:"user_agent,omitempty" db:"user_agent"`
	IP        string `json:"ip,omitempty" db:"ip"`
	DeviceID  string `json:"device_id,omitempty" db:"device_id"`

	// Status
	Status    SessionStatus    `json:"status" db:"status"`
	ExpiresAt shared.Timestamp `json:"expires_at" db:"expires_at"`

	CreatedAt shared.Timestamp  `json:"created_at" db:"created_at"`
	RevokedAt *shared.Timestamp `json:"revoked_at,omitempty" db:"revoked_at"`
}

type AuthMethod string

const (
	AuthMethodPassword  AuthMethod = "PASSWORD"
	AuthMethodAPIKey    AuthMethod = "API_KEY"
	AuthMethodOAuth     AuthMethod = "OAUTH"
	AuthMethodMagicLink AuthMethod = "MAGIC_LINK"
	AuthMethodWebAuthn  AuthMethod = "WEBAUTHN"
)

func (a AuthMethod) String() string {
	return string(a)
}

type SessionStatus string

const (
	SessionStatusActive  SessionStatus = "ACTIVE"
	SessionStatusRevoked SessionStatus = "REVOKED"
	SessionStatusExpired SessionStatus = "EXPIRED"
)

func (s SessionStatus) String() string {
	return string(s)
}

// GenerateReferralCode creates a unique referral code
func GenerateReferralCode() string {
	bytes := make([]byte, 6)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// GenerateDeviceID creates a unique device identifier
func GenerateDeviceID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
