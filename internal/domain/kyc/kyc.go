package kyc

import (
	"time"

	"encrypted-db/internal/domain/shared"
)

// KYCProfile represents a user's KYC verification profile
type KYCProfile struct {
	ID     shared.UUID `json:"id" db:"id"`
	UserID shared.UUID `json:"user_id" db:"user_id"`

	// Status
	Status shared.KYCStatus `json:"status" db:"status"`
	Tier   shared.KYCTier   `json:"tier" db:"tier"`

	// Personal info (encrypted at rest)
	FirstName          string     `json:"first_name,omitempty" db:"first_name_enc"`
	LastName           string     `json:"last_name,omitempty" db:"last_name_enc"`
	DateOfBirth        *time.Time `json:"date_of_birth,omitempty" db:"date_of_birth_enc"`
	Nationality        string     `json:"nationality,omitempty" db:"nationality_enc"`
	CountryOfResidence string     `json:"country_of_residence,omitempty" db:"country_of_residence_enc"`

	// Address
	AddressLine1 string `json:"address_line1,omitempty" db:"address_line1_enc"`
	AddressLine2 string `json:"address_line2,omitempty" db:"address_line2_enc"`
	City         string `json:"city,omitempty" db:"city_enc"`
	State        string `json:"state,omitempty" db:"state_enc"`
	PostalCode   string `json:"postal_code,omitempty" db:"postal_code_enc"`
	Country      string `json:"country,omitempty" db:"country_enc"`

	// Documents
	Documents []KYCDocument `json:"documents,omitempty" db:"-"`

	// Verification
	VerifiedAt *shared.Timestamp `json:"verified_at,omitempty" db:"verified_at"`
	VerifiedBy *shared.UUID      `json:"verified_by,omitempty" db:"verified_by"` // Admin user ID
	ExpiresAt  *time.Time        `json:"expires_at,omitempty" db:"expires_at"`   // KYC expiry

	// Risk
	RiskScore   int      `json:"risk_score" db:"risk_score"`               // 0-100
	RiskFactors []string `json:"risk_factors,omitempty" db:"risk_factors"` // e.g., "PEP", "HIGH_RISK_COUNTRY"

	// Provider info (if using external KYC provider)
	Provider      string `json:"provider,omitempty" db:"provider"` // "sumsub", "onfido", "manual"
	ProviderRefID string `json:"provider_ref_id,omitempty" db:"provider_ref_id"`

	// Notes
	AdminNotes   string `json:"admin_notes,omitempty" db:"admin_notes"`
	RejectReason string `json:"reject_reason,omitempty" db:"reject_reason"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

// IsActive returns true if the KYC profile is approved and not expired
func (p *KYCProfile) IsActive() bool {
	if p.Status != shared.KYCStatusApproved {
		return false
	}
	if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
		return false
	}
	return true
}

// IsExpired returns true if the KYC profile has an expiry date that has passed
func (p *KYCProfile) IsExpired() bool {
	if p.ExpiresAt == nil {
		return false
	}
	return p.ExpiresAt.Before(time.Now())
}

// CanUpgrade returns true if the profile can be upgraded to the requested tier
func (p *KYCProfile) CanUpgrade(targetTier shared.KYCTier) bool {
	if p.Status != shared.KYCStatusApproved {
		return false
	}
	if p.Tier == shared.KYCTierInstitutional {
		return false // Already at max tier
	}
	// Can upgrade if current tier is lower than target
	return p.Tier < targetTier
}

type KYCDocument struct {
	ID           shared.UUID `json:"id" db:"id"`
	KYCProfileID shared.UUID `json:"kyc_profile_id" db:"kyc_profile_id"`

	Type   DocumentType   `json:"type" db:"type"`
	Status DocumentStatus `json:"status" db:"status"`

	// File info
	FileName    string `json:"file_name" db:"file_name"`
	FileSize    int64  `json:"file_size" db:"file_size"`
	MimeType    string `json:"mime_type" db:"mime_type"`
	StoragePath string `json:"storage_path" db:"storage_path"` // S3/GCS path

	// Verification
	VerifiedAt   *shared.Timestamp `json:"verified_at,omitempty" db:"verified_at"`
	VerifiedBy   *shared.UUID      `json:"verified_by,omitempty" db:"verified_by"`
	RejectReason string            `json:"reject_reason,omitempty" db:"reject_reason"`

	// OCR/extracted data (encrypted)
	ExtractedData string `json:"extracted_data,omitempty" db:"extracted_data_enc"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

type DocumentType string

const (
	DocumentTypePassport       DocumentType = "PASSPORT"
	DocumentTypeNationalID     DocumentType = "NATIONAL_ID"
	DocumentTypeDriversLicense DocumentType = "DRIVERS_LICENSE"
	DocumentTypeUtilityBill    DocumentType = "UTILITY_BILL"
	DocumentTypeBankStatement  DocumentType = "BANK_STATEMENT"
	DocumentTypeProofOfAddress DocumentType = "PROOF_OF_ADDRESS"
	DocumentTypeSelfie         DocumentType = "SELFIE"
	DocumentTypeSourceOfFunds  DocumentType = "SOURCE_OF_FUNDS"
	DocumentTypeCorporateDoc   DocumentType = "CORPORATE_DOCUMENT"
)

type DocumentStatus string

const (
	DocumentStatusPending  DocumentStatus = "PENDING"
	DocumentStatusApproved DocumentStatus = "APPROVED"
	DocumentStatusRejected DocumentStatus = "REJECTED"
	DocumentStatusExpired  DocumentStatus = "EXPIRED"
)

func (d DocumentType) String() string {
	return string(d)
}

func (d DocumentStatus) String() string {
	return string(d)
}

// KYCApplication represents a KYC verification request
type KYCApplication struct {
	ID            shared.UUID    `json:"id" db:"id"`
	UserID        shared.UUID    `json:"user_id" db:"user_id"`
	RequestedTier shared.KYCTier `json:"requested_tier" db:"requested_tier"`

	// Status
	Status shared.KYCStatus `json:"status" db:"status"`

	// Form data (encrypted)
	FormData string `json:"form_data,omitempty" db:"form_data_enc"` // JSON encrypted

	// Documents
	Documents []shared.UUID `json:"documents" db:"documents"` // Document IDs

	// Review
	ReviewedAt  *shared.Timestamp `json:"reviewed_at,omitempty" db:"reviewed_at"`
	ReviewedBy  *shared.UUID      `json:"reviewed_by,omitempty" db:"reviewed_by"`
	ReviewNotes string            `json:"review_notes,omitempty" db:"review_notes"`

	// Provider
	Provider      string `json:"provider,omitempty" db:"provider"`
	ProviderRefID string `json:"provider_ref_id,omitempty" db:"provider_ref_id"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

// SanctionsScreening represents a sanctions check result
type SanctionsScreening struct {
	ID             shared.UUID  `json:"id" db:"id"`
	UserID         *shared.UUID `json:"user_id,omitempty" db:"user_id"`
	AddressID      *shared.UUID `json:"address_id,omitempty" db:"address_id"` // For crypto address screening
	CounterpartyID *shared.UUID `json:"counterparty_id,omitempty" db:"counterparty_id"`

	// Screening details
	ScreenType    ScreenType `json:"screen_type" db:"screen_type"`
	Provider      string     `json:"provider" db:"provider"` // "chainalysis", "trm", "ofac", "manual"
	ProviderRefID string     `json:"provider_ref_id,omitempty" db:"provider_ref_id"`

	// Result
	Action         shared.SanctionAction `json:"action" db:"action"`
	RiskScore      float64               `json:"risk_score" db:"risk_score"`                     // 0-100
	MatchedLists   []string              `json:"matched_lists,omitempty" db:"matched_lists"`     // List names
	MatchedEntries []string              `json:"matched_entries,omitempty" db:"matched_entries"` // Specific entries

	// Decision
	DecidedAt *shared.Timestamp `json:"decided_at,omitempty" db:"decided_at"`
	DecidedBy *shared.UUID      `json:"decided_by,omitempty" db:"decided_by"`
	Notes     string            `json:"notes,omitempty" db:"notes"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
}

type ScreenType string

const (
	ScreenTypeUserOnboarding    ScreenType = "USER_ONBOARDING"
	ScreenTypeDepositAddress    ScreenType = "DEPOSIT_ADDRESS"
	ScreenTypeWithdrawalAddress ScreenType = "WITHDRAWAL_ADDRESS"
	ScreenTypeCounterparty      ScreenType = "COUNTERPARTY"
	ScreenTypePeriodicReview    ScreenType = "PERIODIC_REVIEW"
	ScreenTypeTransaction       ScreenType = "TRANSACTION"
)

func (s ScreenType) String() string {
	return string(s)
}

// AMLRule represents an anti-money laundering rule
type AMLRule struct {
	ID          shared.UUID `json:"id" db:"id"`
	Name        string      `json:"name" db:"name"`
	Description string      `json:"description" db:"description"`

	// Rule config
	RuleType AMLRuleType `json:"rule_type" db:"rule_type"`
	Config   string      `json:"config" db:"config"` // JSON config

	// Thresholds
	ThresholdAmount shared.Decimal `json:"threshold_amount,omitempty" db:"threshold_amount"`
	ThresholdCount  int            `json:"threshold_count,omitempty" db:"threshold_count"`
	TimeWindowHours int            `json:"time_window_hours,omitempty" db:"time_window_hours"`

	// Action
	Action   AMLAction `json:"action" db:"action"`     // ALERT, BLOCK, REVIEW
	Severity string    `json:"severity" db:"severity"` // LOW, MEDIUM, HIGH, CRITICAL

	// Status
	Enabled bool `json:"enabled" db:"enabled"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

type AMLRuleType string

const (
	AMLRuleTypeVelocity        AMLRuleType = "VELOCITY"          // Transaction velocity
	AMLRuleTypeStructuring     AMLRuleType = "STRUCTURING"       // Structuring/smurfing
	AMLRuleTypeHighRiskCountry AMLRuleType = "HIGH_RISK_COUNTRY" // High-risk jurisdiction
	AMLRuleTypeMixer           AMLRuleType = "MIXER"             // Mixer/tumbler usage
	AMLRuleTypeDarknet         AMLRuleType = "DARKNET"           // Darknet market interaction
	AMLRuleTypeSanctions       AMLRuleType = "SANCTIONS"         // Sanctions match
	AMLRuleTypePEP             AMLRuleType = "PEP"               // Politically exposed person
	AMLRuleTypeUnusualPattern  AMLRuleType = "UNUSUAL_PATTERN"   // ML-based anomaly
)

func (a AMLRuleType) String() string {
	return string(a)
}

type AMLAction string

const (
	AMLActionAlert  AMLAction = "ALERT"  // Generate alert only
	AMLActionReview AMLAction = "REVIEW" // Queue for manual review
	AMLActionBlock  AMLAction = "BLOCK"  // Block transaction
	AMLActionFreeze AMLAction = "FREEZE" // Freeze account
)

func (a AMLAction) String() string {
	return string(a)
}

// AMLAlert represents a triggered AML rule
type AMLAlert struct {
	ID           shared.UUID  `json:"id" db:"id"`
	RuleID       shared.UUID  `json:"rule_id" db:"rule_id"`
	UserID       shared.UUID  `json:"user_id" db:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty" db:"sub_account_id"`

	// Trigger details
	TriggerData string  `json:"trigger_data" db:"trigger_data"` // JSON snapshot
	RiskScore   float64 `json:"risk_score" db:"risk_score"`

	// Status
	Status     AMLAlertStatus    `json:"status" db:"status"`
	AssignedTo *shared.UUID      `json:"assigned_to,omitempty" db:"assigned_to"`
	ResolvedAt *shared.Timestamp `json:"resolved_at,omitempty" db:"resolved_at"`
	ResolvedBy *shared.UUID      `json:"resolved_by,omitempty" db:"resolved_by"`
	Resolution string            `json:"resolution,omitempty" db:"resolution"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

type AMLAlertStatus string

const (
	AMLAlertStatusOpen          AMLAlertStatus = "OPEN"
	AMLAlertStatusInvestigating AMLAlertStatus = "INVESTIGATING"
	AMLAlertStatusResolved      AMLAlertStatus = "RESOLVED"
	AMLAlertStatusDismissed     AMLAlertStatus = "DISMISSED"
	AMLAlertStatusEscalated     AMLAlertStatus = "ESCALATED"
)

func (a AMLAlertStatus) String() string {
	return string(a)
}
