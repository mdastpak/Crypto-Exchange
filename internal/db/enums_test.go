package db_test

import (
	"fmt"
	"sort"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/account"
	"github.com/mdastpak/crypto-exchange/internal/domain/custody"
	"github.com/mdastpak/crypto-exchange/internal/domain/kyc"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func values(constants ...interface{}) []string {
	out := make([]string, len(constants))
	for i, c := range constants {
		out[i] = fmt.Sprint(c)
	}
	sort.Strings(out)
	return out
}

// goEnums maps each database enum to the constants of its Go type. A value
// that the Go code can write but the database refuses fails at the first
// insert; a value that the database holds but Go does not know is misread.
var goEnums = map[string][]string{
	"account_status_type": values(account.UserStatusActive, account.UserStatusInactive, account.UserStatusSuspended, account.UserStatusBanned, account.UserStatusPending),
	"contact_type":        values(account.ContactEmail, account.ContactMobile, account.ContactUsername),
	"sub_account_status":  values(account.SubAccountStatusActive, account.SubAccountStatusInactive, account.SubAccountStatusFrozen),
	"api_key_type":        values(account.APIKeyTypeHMAC, account.APIKeyTypeEd25519),
	"api_key_status":      values(account.APIKeyStatusActive, account.APIKeyStatusInactive, account.APIKeyStatusRevoked, account.APIKeyStatusExpired),
	"auth_method":         values(account.AuthMethodPassword, account.AuthMethodAPIKey, account.AuthMethodOAuth, account.AuthMethodMagicLink, account.AuthMethodWebAuthn),
	"session_status":      values(account.SessionStatusActive, account.SessionStatusRevoked, account.SessionStatusExpired),

	"custody_provider_type": values(custody.CustodyProviderInternal, custody.CustodyProviderBTC, custody.CustodyProviderEVM, custody.CustodyProviderSolana, custody.CustodyProviderFiat),
	"deposit_status_type":   values(custody.DepositStatusDetected, custody.DepositStatusConfirming, custody.DepositStatusConfirmed, custody.DepositStatusCredited, custody.DepositStatusFailed),
	"fee_level":             values(custody.FeeLevelSlow, custody.FeeLevelNormal, custody.FeeLevelFast, custody.FeeLevelCustom),
	"withdrawal_status_type": values(custody.WithdrawalStatusPending, custody.WithdrawalStatusBroadcast, custody.WithdrawalStatusConfirming, custody.WithdrawalStatusCompleted,
		custody.WithdrawalStatusFailed, custody.WithdrawalStatusCancelled, custody.WithdrawalStatusRejected),
	"settlement_type": values(custody.SettlementTypeTrade, custody.SettlementTypeWithdrawal, custody.SettlementTypeDeposit, custody.SettlementTypeTransfer,
		custody.SettlementTypeFee, custody.SettlementTypeFunding, custody.SettlementTypeLiquidation),
	"settlement_status": values(custody.SettlementStatusPending, custody.SettlementStatusProcessing, custody.SettlementStatusCompleted, custody.SettlementStatusFailed, custody.SettlementStatusCancelled),

	"document_type": values(kyc.DocumentTypePassport, kyc.DocumentTypeNationalID, kyc.DocumentTypeDriversLicense, kyc.DocumentTypeUtilityBill, kyc.DocumentTypeBankStatement,
		kyc.DocumentTypeProofOfAddress, kyc.DocumentTypeSelfie, kyc.DocumentTypeSourceOfFunds, kyc.DocumentTypeCorporateDoc),
	"document_status": values(kyc.DocumentStatusPending, kyc.DocumentStatusApproved, kyc.DocumentStatusRejected, kyc.DocumentStatusExpired),
	"screen_type": values(kyc.ScreenTypeUserOnboarding, kyc.ScreenTypeDepositAddress, kyc.ScreenTypeWithdrawalAddress, kyc.ScreenTypeCounterparty,
		kyc.ScreenTypePeriodicReview, kyc.ScreenTypeTransaction),
	"aml_rule_type": values(kyc.AMLRuleTypeVelocity, kyc.AMLRuleTypeStructuring, kyc.AMLRuleTypeHighRiskCountry, kyc.AMLRuleTypeMixer, kyc.AMLRuleTypeDarknet,
		kyc.AMLRuleTypeSanctions, kyc.AMLRuleTypePEP, kyc.AMLRuleTypeUnusualPattern),
	"aml_action":       values(kyc.AMLActionAlert, kyc.AMLActionReview, kyc.AMLActionBlock, kyc.AMLActionFreeze),
	"aml_alert_status": values(kyc.AMLAlertStatusOpen, kyc.AMLAlertStatusInvestigating, kyc.AMLAlertStatusResolved, kyc.AMLAlertStatusDismissed, kyc.AMLAlertStatusEscalated),

	"market_status": values(market.MarketStatusActive, market.MarketStatusInactive, market.MarketStatusMaintenance, market.MarketStatusSuspended, market.MarketStatusDelisted),

	"asset_type":      values(shared.AssetTypeFiat, shared.AssetTypeCoin, shared.AssetTypeToken, shared.AssetTypeStable, shared.AssetTypeWrapped),
	"asset_status":    values(shared.AssetStatusActive, shared.AssetStatusInactive, shared.AssetStatusDeprecated, shared.AssetStatusDelisted),
	"order_side":      values(shared.OrderSideBuy, shared.OrderSideSell),
	"order_type":      values(shared.OrderTypeMarket, shared.OrderTypeLimit, shared.OrderTypeStop, shared.OrderTypeStopLimit),
	"order_status":    values(shared.OrderStatusNew, shared.OrderStatusPartiallyFilled, shared.OrderStatusFilled, shared.OrderStatusCancelled, shared.OrderStatusRejected, shared.OrderStatusExpired),
	"time_in_force":   values(shared.TimeInForceGTC, shared.TimeInForceIOC, shared.TimeInForceFOK, shared.TimeInForceGTX),
	"trade_side":      values(shared.TradeSideBuy, shared.TradeSideSell),
	"settlement_mode": values(shared.SettlementModeOffChain, shared.SettlementModeOnChain),
	"kyc_status":      values(shared.KYCStatusPending, shared.KYCStatusApproved, shared.KYCStatusRejected, shared.KYCStatusExpired, shared.KYCStatusUnderReview),
	"kyc_tier":        values(shared.KYCTierNone, shared.KYCTierBasic, shared.KYCTierStandard, shared.KYCTierEnhanced, shared.KYCTierInstitutional),
	"sanction_action": values(shared.SanctionActionAllow, shared.SanctionActionReview, shared.SanctionActionBlock),

	// The status of currencies.
	"status_enum": values(models.CurrencyStatusApproved, models.CurrencyStatusPending, models.CurrencyStatusSuspend, models.CurrencyStatusDeleted),
}

// TestEnums_MatchTheDatabase compares every enum that the migrations create
// with its Go type, in both directions, and fails for a database enum that
// has no Go type here.
func TestEnums_MatchTheDatabase(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_enums")

	rows, err := conn.Query(`
		SELECT t.typname, e.enumlabel
		FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public'`)
	require.NoError(t, err)
	defer rows.Close()
	database := make(map[string][]string)
	for rows.Next() {
		var name, label string
		require.NoError(t, rows.Scan(&name, &label))
		database[name] = append(database[name], label)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, database)

	for name, labels := range database {
		sort.Strings(labels)
		want, ok := goEnums[name]
		if !assert.True(t, ok, "the database enum %s has no Go type in goEnums", name) {
			continue
		}
		assert.Equal(t, labels, want, "database enum %s (left) and its Go constants (right)", name)
	}
	for name := range goEnums {
		assert.Contains(t, database, name, "goEnums lists %s, which the migrations do not create", name)
	}
}
