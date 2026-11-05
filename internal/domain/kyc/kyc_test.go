package kyc

import (
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestKYCProfile_IsActive(t *testing.T) {
	profile := &KYCProfile{
		Status: shared.KYCStatusApproved,
	}
	assert.True(t, profile.IsActive())

	profile2 := &KYCProfile{
		Status: shared.KYCStatusPending,
	}
	assert.False(t, profile2.IsActive())
}

func TestKYCProfile_IsExpired(t *testing.T) {
	expired := time.Now().Add(-24 * time.Hour)
	profile := &KYCProfile{
		ExpiresAt: &expired,
	}
	assert.True(t, profile.IsExpired())

	future := time.Now().Add(24 * time.Hour)
	profile2 := &KYCProfile{
		ExpiresAt: &future,
	}
	assert.False(t, profile2.IsExpired())

	// No expiry
	profile3 := &KYCProfile{}
	assert.False(t, profile3.IsExpired())
}

// tiers lists every tier, lowest first.
var tiers = []shared.KYCTier{
	shared.KYCTierNone, shared.KYCTierBasic, shared.KYCTierStandard, shared.KYCTierEnhanced, shared.KYCTierInstitutional,
}

var statuses = []shared.KYCStatus{
	shared.KYCStatusPending, shared.KYCStatusApproved, shared.KYCStatusRejected, shared.KYCStatusExpired, shared.KYCStatusUnderReview,
}

func TestKYCTier_Rank(t *testing.T) {
	for i, tier := range tiers {
		rank, ok := tier.Rank()
		assert.True(t, ok, tier)
		assert.Equal(t, i, rank, tier)
	}
	for _, unknown := range []shared.KYCTier{"", "basic", "PLATINUM"} {
		_, ok := unknown.Rank()
		assert.False(t, ok, "%q", unknown)
	}
}

// TestKYCProfile_CanUpgrade checks every pair of tiers with every status.
// As strings, "BASIC" < "NONE", so a string comparison let a user at BASIC
// "upgrade" to NONE and kept a new user at NONE from ever upgrading.
func TestKYCProfile_CanUpgrade(t *testing.T) {
	for currentRank, current := range tiers {
		for targetRank, target := range tiers {
			for _, status := range statuses {
				var want bool
				switch {
				case targetRank <= currentRank:
					want = false // never sideways or down
				case current == shared.KYCTierNone:
					want = status != shared.KYCStatusUnderReview
				default:
					want = status == shared.KYCStatusApproved
				}
				profile := &KYCProfile{Tier: current, Status: status}
				assert.Equal(t, want, profile.CanUpgrade(target), "%s (%s) to %s", current, status, target)
			}
		}
	}

	newUser := &KYCProfile{Tier: shared.KYCTierNone, Status: shared.KYCStatusPending}
	assert.True(t, newUser.CanUpgrade(shared.KYCTierBasic), "a new user can start their KYC")
	assert.False(t, (&KYCProfile{Tier: shared.KYCTierBasic, Status: shared.KYCStatusApproved}).CanUpgrade(shared.KYCTierNone))
	assert.False(t, (&KYCProfile{Tier: "PLATINUM", Status: shared.KYCStatusApproved}).CanUpgrade(shared.KYCTierInstitutional), "an unknown current tier")
	assert.False(t, (&KYCProfile{Tier: shared.KYCTierBasic, Status: shared.KYCStatusApproved}).CanUpgrade("PLATINUM"), "an unknown target tier")
}

func TestKYCDocument(t *testing.T) {
	doc := &KYCDocument{
		ID:           shared.NewUUID(),
		KYCProfileID: shared.NewUUID(),
		Type:         DocumentTypePassport,
		Status:       DocumentStatusPending,
		FileName:     "passport.jpg",
		FileSize:     1024000,
		MimeType:     "image/jpeg",
		StoragePath:  "s3://bucket/kyc/passport.jpg",
		CreatedAt:    shared.Timestamp{},
	}

	assert.Equal(t, DocumentTypePassport, doc.Type)
	assert.Equal(t, DocumentStatusPending, doc.Status)
	assert.Equal(t, int64(1024000), doc.FileSize)
}

func TestKYCDocument_StatusTransitions(t *testing.T) {
	doc := &KYCDocument{Status: DocumentStatusPending}
	assert.Equal(t, DocumentStatusPending, doc.Status)

	doc.Status = DocumentStatusApproved
	assert.Equal(t, DocumentStatusApproved, doc.Status)

	doc.Status = DocumentStatusRejected
	assert.Equal(t, DocumentStatusRejected, doc.Status)

	doc.Status = DocumentStatusExpired
	assert.Equal(t, DocumentStatusExpired, doc.Status)
}

func TestDocumentType_String(t *testing.T) {
	assert.Equal(t, "PASSPORT", DocumentTypePassport.String())
	assert.Equal(t, "NATIONAL_ID", DocumentTypeNationalID.String())
	assert.Equal(t, "DRIVERS_LICENSE", DocumentTypeDriversLicense.String())
	assert.Equal(t, "UTILITY_BILL", DocumentTypeUtilityBill.String())
	assert.Equal(t, "BANK_STATEMENT", DocumentTypeBankStatement.String())
	assert.Equal(t, "PROOF_OF_ADDRESS", DocumentTypeProofOfAddress.String())
	assert.Equal(t, "SELFIE", DocumentTypeSelfie.String())
	assert.Equal(t, "SOURCE_OF_FUNDS", DocumentTypeSourceOfFunds.String())
	assert.Equal(t, "CORPORATE_DOCUMENT", DocumentTypeCorporateDoc.String())
}

func TestDocumentStatus_String(t *testing.T) {
	assert.Equal(t, "PENDING", DocumentStatusPending.String())
	assert.Equal(t, "APPROVED", DocumentStatusApproved.String())
	assert.Equal(t, "REJECTED", DocumentStatusRejected.String())
	assert.Equal(t, "EXPIRED", DocumentStatusExpired.String())
}

func TestKYCApplication(t *testing.T) {
	app := &KYCApplication{
		ID:            shared.NewUUID(),
		UserID:        shared.NewUUID(),
		RequestedTier: shared.KYCTierEnhanced,
		Status:        shared.KYCStatusPending,
		FormData:      `{"first_name":"John","last_name":"Doe"}`,
		Documents:     []shared.UUID{shared.NewUUID(), shared.NewUUID()},
		Provider:      "sumsub",
		CreatedAt:     shared.Timestamp{},
	}

	assert.Equal(t, shared.KYCTierEnhanced, app.RequestedTier)
	assert.Equal(t, shared.KYCStatusPending, app.Status)
	assert.Len(t, app.Documents, 2)
}

func TestKYCApplication_StatusTransitions(t *testing.T) {
	app := &KYCApplication{Status: shared.KYCStatusPending}
	assert.Equal(t, shared.KYCStatusPending, app.Status)

	app.Status = shared.KYCStatusApproved
	assert.Equal(t, shared.KYCStatusApproved, app.Status)

	app.Status = shared.KYCStatusRejected
	assert.Equal(t, shared.KYCStatusRejected, app.Status)
}

func TestSanctionsScreening(t *testing.T) {
	screening := &SanctionsScreening{
		ID:             shared.NewUUID(),
		UserID:         func() *shared.UUID { id := shared.NewUUID(); return &id }(),
		ScreenType:     ScreenTypeUserOnboarding,
		Provider:       "chainalysis",
		ProviderRefID:  "ref-123",
		Action:         shared.SanctionActionAllow,
		RiskScore:      10.5,
		MatchedLists:   []string{},
		MatchedEntries: []string{},
		CreatedAt:      shared.Timestamp{},
	}

	assert.Equal(t, ScreenTypeUserOnboarding, screening.ScreenType)
	assert.Equal(t, shared.SanctionActionAllow, screening.Action)
	assert.Equal(t, 10.5, screening.RiskScore)
	assert.Empty(t, screening.MatchedLists)
}

func TestSanctionsScreening_Match(t *testing.T) {
	screening := &SanctionsScreening{
		ScreenType:     ScreenTypeWithdrawalAddress,
		Provider:       "ofac",
		Action:         shared.SanctionActionBlock,
		RiskScore:      95.0,
		MatchedLists:   []string{"SDN", "NS-PLC"},
		MatchedEntries: []string{"John Doe - SDN Entry 12345"},
	}

	assert.Equal(t, shared.SanctionActionBlock, screening.Action)
	assert.Equal(t, 95.0, screening.RiskScore)
	assert.Contains(t, screening.MatchedLists, "SDN")
	assert.Contains(t, screening.MatchedEntries, "John Doe - SDN Entry 12345")
}

func TestScreenType_String(t *testing.T) {
	assert.Equal(t, "USER_ONBOARDING", ScreenTypeUserOnboarding.String())
	assert.Equal(t, "DEPOSIT_ADDRESS", ScreenTypeDepositAddress.String())
	assert.Equal(t, "WITHDRAWAL_ADDRESS", ScreenTypeWithdrawalAddress.String())
	assert.Equal(t, "COUNTERPARTY", ScreenTypeCounterparty.String())
	assert.Equal(t, "PERIODIC_REVIEW", ScreenTypePeriodicReview.String())
	assert.Equal(t, "TRANSACTION", ScreenTypeTransaction.String())
}

func TestAMLRule(t *testing.T) {
	rule := &AMLRule{
		ID:              shared.NewUUID(),
		Name:            "High Velocity Transactions",
		Description:     "Detect rapid successive transactions",
		RuleType:        AMLRuleTypeVelocity,
		Config:          `{"max_tx_per_hour": 50}`,
		ThresholdAmount: shared.MustNewDecimalFromString("10000"),
		ThresholdCount:  10,
		TimeWindowHours: 1,
		Action:          AMLActionAlert,
		Severity:        "HIGH",
		Enabled:         true,
		CreatedAt:       shared.Timestamp{},
	}

	assert.Equal(t, AMLRuleTypeVelocity, rule.RuleType)
	assert.Equal(t, AMLActionAlert, rule.Action)
	assert.Equal(t, "HIGH", rule.Severity)
	assert.True(t, rule.Enabled)
}

func TestAMLRule_Disabled(t *testing.T) {
	rule := &AMLRule{
		RuleType: AMLRuleTypeStructuring,
		Enabled:  false,
	}
	assert.False(t, rule.Enabled)
}

func TestAMLRuleType_String(t *testing.T) {
	assert.Equal(t, "VELOCITY", AMLRuleTypeVelocity.String())
	assert.Equal(t, "STRUCTURING", AMLRuleTypeStructuring.String())
	assert.Equal(t, "HIGH_RISK_COUNTRY", AMLRuleTypeHighRiskCountry.String())
	assert.Equal(t, "MIXER", AMLRuleTypeMixer.String())
	assert.Equal(t, "DARKNET", AMLRuleTypeDarknet.String())
	assert.Equal(t, "SANCTIONS", AMLRuleTypeSanctions.String())
	assert.Equal(t, "PEP", AMLRuleTypePEP.String())
	assert.Equal(t, "UNUSUAL_PATTERN", AMLRuleTypeUnusualPattern.String())
}

func TestAMLAction_String(t *testing.T) {
	assert.Equal(t, "ALERT", AMLActionAlert.String())
	assert.Equal(t, "REVIEW", AMLActionReview.String())
	assert.Equal(t, "BLOCK", AMLActionBlock.String())
	assert.Equal(t, "FREEZE", AMLActionFreeze.String())
}

func TestAMLAlert(t *testing.T) {
	alert := &AMLAlert{
		ID:          shared.NewUUID(),
		RuleID:      shared.NewUUID(),
		UserID:      shared.NewUUID(),
		TriggerData: `{"tx_count": 15, "time_window": "1h"}`,
		RiskScore:   75.0,
		Status:      AMLAlertStatusOpen,
		CreatedAt:   shared.Timestamp{},
	}

	assert.Equal(t, AMLAlertStatusOpen, alert.Status)
	assert.Equal(t, 75.0, alert.RiskScore)
	assert.Contains(t, alert.TriggerData, "tx_count")
}

func TestAMLAlert_StatusTransitions(t *testing.T) {
	alert := &AMLAlert{Status: AMLAlertStatusOpen}
	assert.Equal(t, AMLAlertStatusOpen, alert.Status)

	alert.Status = AMLAlertStatusInvestigating
	assert.Equal(t, AMLAlertStatusInvestigating, alert.Status)

	alert.Status = AMLAlertStatusResolved
	assert.Equal(t, AMLAlertStatusResolved, alert.Status)

	alert.Status = AMLAlertStatusDismissed
	assert.Equal(t, AMLAlertStatusDismissed, alert.Status)

	alert.Status = AMLAlertStatusEscalated
	assert.Equal(t, AMLAlertStatusEscalated, alert.Status)
}

func TestAMLAlertStatus_String(t *testing.T) {
	assert.Equal(t, "OPEN", AMLAlertStatusOpen.String())
	assert.Equal(t, "INVESTIGATING", AMLAlertStatusInvestigating.String())
	assert.Equal(t, "RESOLVED", AMLAlertStatusResolved.String())
	assert.Equal(t, "DISMISSED", AMLAlertStatusDismissed.String())
	assert.Equal(t, "ESCALATED", AMLAlertStatusEscalated.String())
}
