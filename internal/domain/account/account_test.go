package account

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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
	key, err := GenerateAPIKey(APIKeyTypeHMAC)
	require.NoError(t, err)
	assert.Equal(t, APIKeyTypeHMAC, key.Type)
	assert.Regexp(t, `^hmac_[0-9a-f]{32}$`, key.KeyID)
	secret, err := base64.RawURLEncoding.DecodeString(key.Secret)
	require.NoError(t, err)
	assert.Len(t, secret, 32, "a 256-bit secret")
	assert.Nil(t, key.PublicKey)
	assert.NotContains(t, key.KeyID, hex.EncodeToString(secret[:4]), "the key ID must not reveal the secret")
}

func TestGenerateAPIKey_Ed25519(t *testing.T) {
	key, err := GenerateAPIKey(APIKeyTypeEd25519)
	require.NoError(t, err)
	assert.Regexp(t, `^ed25519_[0-9a-f]{32}$`, key.KeyID)
	seed, err := base64.RawURLEncoding.DecodeString(key.Secret)
	require.NoError(t, err)
	require.Len(t, seed, ed25519.SeedSize)

	// The client signs with the private key from the seed, and the server
	// verifies with the public key it stored.
	private := ed25519.NewKeyFromSeed(seed)
	assert.Equal(t, key.PublicKey, private.Public(), "the public key belongs to the seed")
	message := []byte("GET /user/profile 1730800000")
	assert.True(t, VerifyEd25519Signature(key.PublicKey, message, ed25519.Sign(private, message)))
	assert.NotContains(t, key.KeyID, hex.EncodeToString(seed[:4]))
}

func TestGenerateAPIKey_KeyIDsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		key, err := GenerateAPIKey(APIKeyTypeHMAC)
		require.NoError(t, err)
		require.False(t, seen[key.KeyID], "duplicate key ID")
		seen[key.KeyID] = true
	}
}

func TestGenerateAPIKey_InvalidType(t *testing.T) {
	_, err := GenerateAPIKey("INVALID")
	assert.EqualError(t, err, "unsupported key type")
	_, err = NewAPIKeyID("RSA")
	assert.Error(t, err)
}

func TestParseEd25519PublicKey(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(public),
		base64.RawStdEncoding.EncodeToString(public),
		base64.URLEncoding.EncodeToString(public),
		" " + base64.RawURLEncoding.EncodeToString(public) + "\n",
	} {
		got, err := ParseEd25519PublicKey(encoded)
		require.NoError(t, err, encoded)
		assert.Equal(t, public, got)
	}
	for _, encoded := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(public[:31]), base64.StdEncoding.EncodeToString(append(public, 0))} {
		_, err := ParseEd25519PublicKey(encoded)
		assert.Error(t, err, encoded)
	}
}

func TestVerifyHMACSignature(t *testing.T) {
	secret, message := []byte("shared secret"), []byte("POST /orders 1730800000 {}")
	mac := hmac.New(sha256.New, secret)
	mac.Write(message)
	signature := mac.Sum(nil)

	assert.True(t, VerifyHMACSignature(secret, message, signature))
	assert.False(t, VerifyHMACSignature([]byte("other secret"), message, signature))
	assert.False(t, VerifyHMACSignature(secret, []byte("POST /orders 1730800001 {}"), signature))
	assert.False(t, VerifyHMACSignature(secret, message, signature[:31]), "a truncated signature")
	assert.False(t, VerifyHMACSignature(secret, message, nil))
}

func TestVerifyEd25519Signature_RejectsMalformedInput(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	message := []byte("message")
	signature := ed25519.Sign(private, message)

	assert.False(t, VerifyEd25519Signature(public, []byte("other"), signature))
	assert.False(t, VerifyEd25519Signature(public[:31], message, signature), "a short key does not panic")
	assert.False(t, VerifyEd25519Signature(public, message, signature[:63]))
	assert.False(t, VerifyEd25519Signature(nil, message, signature))
}

// recordingSealer records the associated data and seals with a fixed,
// reversible transformation, to check what SealAPIKeySecret binds.
type recordingSealer struct{ aads []string }

func (s *recordingSealer) SealField(aad, plaintext []byte) ([]byte, error) {
	s.aads = append(s.aads, string(aad))
	return append([]byte(string(aad)+"|"), plaintext...), nil
}

func (s *recordingSealer) OpenField(aad, sealed []byte) ([]byte, error) {
	s.aads = append(s.aads, string(aad))
	prefix := string(aad) + "|"
	if !strings.HasPrefix(string(sealed), prefix) {
		return nil, errors.New("does not decrypt")
	}
	return sealed[len(prefix):], nil
}

func TestSealAPIKeySecret_BindsTheSecretToItsKey(t *testing.T) {
	sealer := &recordingSealer{}
	sealed, err := SealAPIKeySecret(sealer, "hmac_aa", []byte("secret"))
	require.NoError(t, err)

	opened, err := OpenAPIKeySecret(sealer, "hmac_aa", sealed)
	require.NoError(t, err)
	assert.Equal(t, "secret", string(opened))
	_, err = OpenAPIKeySecret(sealer, "hmac_bb", sealed)
	assert.Error(t, err, "a secret copied to another key's row does not open")
	assert.Equal(t, []string{"api_keys.secret_sealed:hmac_aa", "api_keys.secret_sealed:hmac_aa", "api_keys.secret_sealed:hmac_bb"}, sealer.aads)
}

func TestAPIKey_NeverSerializesItsSecrets(t *testing.T) {
	encoded, err := json.Marshal(APIKey{PublicKey: "hmac_aa", SealedSecret: []byte("sealed"), Ed25519PublicKey: []byte("public")})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "c2VhbGVk", "the sealed secret, base64-encoded")
	assert.NotContains(t, string(encoded), "cHVibGlj")
	assert.Contains(t, string(encoded), `"public_key":"hmac_aa"`)
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
