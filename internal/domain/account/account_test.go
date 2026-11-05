package account

import (
	"errors"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUser_IsActive(t *testing.T) {
	activeUser := &User{Status: UserStatusActive}
	inactiveUser := &User{Status: UserStatusInactive}
	suspendedUser := &User{Status: UserStatusSuspended}
	bannedUser := &User{Status: UserStatusBanned}
	pendingUser := &User{Status: UserStatusPending}

	assert.True(t, activeUser.IsActive())
	assert.False(t, inactiveUser.IsActive())
	assert.False(t, suspendedUser.IsActive())
	assert.False(t, bannedUser.IsActive())
	assert.False(t, pendingUser.IsActive())
}

func TestUser_CanTrade(t *testing.T) {
	user := &User{
		Status:    UserStatusActive,
		KYCStatus: shared.KYCStatusApproved,
	}

	assert.True(t, user.CanTrade())

	// Inactive user
	user2 := &User{
		Status:    UserStatusInactive,
		KYCStatus: shared.KYCStatusApproved,
	}
	assert.False(t, user2.CanTrade())

	// Active but KYC not approved
	user3 := &User{
		Status:    UserStatusActive,
		KYCStatus: shared.KYCStatusPending,
	}
	assert.False(t, user3.CanTrade())

	// Active but KYC rejected
	user4 := &User{
		Status:    UserStatusActive,
		KYCStatus: shared.KYCStatusRejected,
	}
	assert.False(t, user4.CanTrade())
}

func TestUserStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", UserStatusActive.String())
	assert.Equal(t, "INACTIVE", UserStatusInactive.String())
	assert.Equal(t, "SUSPENDED", UserStatusSuspended.String())
	assert.Equal(t, "BANNED", UserStatusBanned.String())
	assert.Equal(t, "PENDING_VERIFICATION", UserStatusPending.String())
}

func TestUserStatus_CanSignIn(t *testing.T) {
	for status, want := range map[UserStatus]bool{
		UserStatusActive:    true,
		UserStatusPending:   true,
		UserStatusInactive:  false,
		UserStatusSuspended: false,
		UserStatusBanned:    false,
		"":                  false,
	} {
		assert.Equal(t, want, status.CanSignIn(), "status %q", status)
	}
}

func TestUser_PrimaryContact(t *testing.T) {
	user := &User{Contacts: []Contact{
		{Type: ContactEmail, Value: "old@example.com"},
		{Type: ContactEmail, Value: "new@example.com", IsPrimary: true},
		{Type: ContactUsername, Value: "legacy", IsPrimary: true},
	}}

	email, ok := user.PrimaryContact(ContactEmail)
	assert.True(t, ok)
	assert.Equal(t, "new@example.com", email.Value, "a contact that is not primary is skipped")
	_, ok = user.PrimaryContact(ContactMobile)
	assert.False(t, ok)
}

func TestSubAccount_IsActive(t *testing.T) {
	active := &SubAccount{Status: SubAccountStatusActive}
	inactive := &SubAccount{Status: SubAccountStatusInactive}
	frozen := &SubAccount{Status: SubAccountStatusFrozen}

	assert.True(t, active.IsActive())
	assert.False(t, inactive.IsActive())
	assert.False(t, frozen.IsActive())
}

func TestSubAccount_HasPermission(t *testing.T) {
	subAcc := &SubAccount{
		Permissions: []Permission{
			PermissionReadAccount,
			PermissionPlaceOrder,
			PermissionCancelOrder,
		},
	}

	assert.True(t, subAcc.HasPermission(PermissionReadAccount))
	assert.True(t, subAcc.HasPermission(PermissionPlaceOrder))
	assert.True(t, subAcc.HasPermission(PermissionCancelOrder))
	assert.False(t, subAcc.HasPermission(PermissionWithdraw))
	assert.False(t, subAcc.HasPermission(PermissionAdmin))
}

func TestSubAccountStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", SubAccountStatusActive.String())
	assert.Equal(t, "INACTIVE", SubAccountStatusInactive.String())
	assert.Equal(t, "FROZEN", SubAccountStatusFrozen.String())
}

func TestPermission_String(t *testing.T) {
	assert.Equal(t, "READ_ACCOUNT", PermissionReadAccount.String())
	assert.Equal(t, "READ_BALANCES", PermissionReadBalances.String())
	assert.Equal(t, "READ_ORDERS", PermissionReadOrders.String())
	assert.Equal(t, "READ_TRADES", PermissionReadTrades.String())
	assert.Equal(t, "READ_POSITIONS", PermissionReadPositions.String())
	assert.Equal(t, "READ_MARKET_DATA", PermissionReadMarketData.String())
	assert.Equal(t, "PLACE_ORDER", PermissionPlaceOrder.String())
	assert.Equal(t, "CANCEL_ORDER", PermissionCancelOrder.String())
	assert.Equal(t, "MODIFY_ORDER", PermissionModifyOrder.String())
	assert.Equal(t, "WITHDRAW", PermissionWithdraw.String())
	assert.Equal(t, "TRANSFER", PermissionTransfer.String())
	assert.Equal(t, "ADMIN", PermissionAdmin.String())
}

func TestAPIKeyType_String(t *testing.T) {
	assert.Equal(t, "HMAC", APIKeyTypeHMAC.String())
	assert.Equal(t, "ED25519", APIKeyTypeEd25519.String())
}

func TestAPIKeyStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", APIKeyStatusActive.String())
	assert.Equal(t, "INACTIVE", APIKeyStatusInactive.String())
	assert.Equal(t, "REVOKED", APIKeyStatusRevoked.String())
	assert.Equal(t, "EXPIRED", APIKeyStatusExpired.String())
}

func TestGenerateAPIKey_HMAC(t *testing.T) {
	publicKey, secret, err := GenerateAPIKey(APIKeyTypeHMAC)
	require.NoError(t, err)
	assert.NotEmpty(t, publicKey)
	assert.NotEmpty(t, secret)
	assert.True(t, len(publicKey) > 4) // "hmac_" + 8 chars
	assert.True(t, len(secret) > 0)
}

func TestGenerateAPIKey_Ed25519(t *testing.T) {
	publicKey, secret, err := GenerateAPIKey(APIKeyTypeEd25519)
	require.NoError(t, err)
	assert.NotEmpty(t, publicKey)
	assert.NotEmpty(t, secret)
	assert.True(t, len(publicKey) > 4) // "ed25519_" + 8 chars
}

func TestGenerateAPIKey_InvalidType(t *testing.T) {
	_, _, err := GenerateAPIKey("INVALID")
	assert.Error(t, err)
	assert.Equal(t, errors.New("unsupported key type"), err)
}

func TestHashAPIKeySecret(t *testing.T) {
	secret := "test-secret"
	hash, err := HashAPIKeySecret(secret)
	require.NoError(t, err)
	// In test implementation, it just returns the secret
	assert.Equal(t, secret, hash)
}

func TestVerifyAPIKeySecret(t *testing.T) {
	secret := "test-secret"
	hash := "test-secret"

	assert.True(t, VerifyAPIKeySecret(secret, hash))
	assert.False(t, VerifyAPIKeySecret("wrong-secret", hash))
}

func TestAuthMethod_String(t *testing.T) {
	assert.Equal(t, "PASSWORD", AuthMethodPassword.String())
	assert.Equal(t, "API_KEY", AuthMethodAPIKey.String())
	assert.Equal(t, "OAUTH", AuthMethodOAuth.String())
	assert.Equal(t, "MAGIC_LINK", AuthMethodMagicLink.String())
	assert.Equal(t, "WEBAUTHN", AuthMethodWebAuthn.String())
}

func TestSessionStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", SessionStatusActive.String())
	assert.Equal(t, "REVOKED", SessionStatusRevoked.String())
	assert.Equal(t, "EXPIRED", SessionStatusExpired.String())
}

func TestGenerateReferralCode(t *testing.T) {
	code1 := GenerateReferralCode()
	code2 := GenerateReferralCode()

	assert.NotEmpty(t, code1)
	assert.NotEmpty(t, code2)
	assert.NotEqual(t, code1, code2) // Very unlikely to be equal
	assert.Len(t, code1, 12)         // 6 bytes = 12 hex chars
}

func TestGenerateDeviceID(t *testing.T) {
	id1 := GenerateDeviceID()
	id2 := GenerateDeviceID()

	assert.NotEmpty(t, id1)
	assert.NotEmpty(t, id2)
	assert.NotEqual(t, id1, id2)
	assert.Len(t, id1, 32) // 16 bytes = 32 hex chars
}
