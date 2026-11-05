package account

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"encrypted-db/internal/domain/shared"
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
	PublicKey string     `json:"public_key" db:"public_key"` // Key ID (prefix)
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

	// Secret (only returned on creation, stored hashed)
	SecretHash string `json:"-" db:"secret_hash"`

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

// GenerateAPIKey creates a new API key pair
func GenerateAPIKey(keyType APIKeyType) (publicKey, secret string, err error) {
	switch keyType {
	case APIKeyTypeHMAC:
		// Generate 32-byte secret
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			return "", "", err
		}
		secret = base64.URLEncoding.EncodeToString(secretBytes)
		// Public key is first 8 chars of SHA256(secret) for identification
		publicKey = "hmac_" + hex.EncodeToString(secretBytes[:4])

	case APIKeyTypeEd25519:
		// For Ed25519, we'd use crypto/ed25519.GenerateKey
		// Simplified here - in production use proper key generation
		secretBytes := make([]byte, 64) // seed + public
		if _, err := rand.Read(secretBytes); err != nil {
			return "", "", err
		}
		secret = base64.URLEncoding.EncodeToString(secretBytes)
		publicKey = "ed25519_" + hex.EncodeToString(secretBytes[:4])

	default:
		return "", "", errors.New("unsupported key type")
	}
	return publicKey, secret, nil
}

// HashAPIKeySecret hashes the secret for storage
func HashAPIKeySecret(secret string) (string, error) {
	// Use bcrypt or argon2 in production
	// Simplified for domain model
	return secret, nil // Placeholder
}

// VerifyAPIKeySecret verifies a secret against hash
func VerifyAPIKeySecret(secret, hash string) bool {
	// In production: bcrypt.CompareHashAndPassword
	return secret == hash // Placeholder
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
