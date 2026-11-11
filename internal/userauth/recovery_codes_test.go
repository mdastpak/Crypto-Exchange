package userauth

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryCodes_AreRandomAndReadable(t *testing.T) {
	format := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}(-[A-HJ-NP-Z2-9]{4}){3}$`)
	seen := map[string]bool{}
	for range 200 {
		code := newRecoveryCode()
		require.Regexp(t, format, code)
		require.False(t, seen[code])
		seen[code] = true
	}
}

func TestNormalizeRecoveryCode_AcceptsWhatUsersType(t *testing.T) {
	want, ok := normalizeRecoveryCode("ABCD-EFGH-JKLM-NPQR")
	require.True(t, ok)
	assert.Equal(t, "ABCDEFGHJKLMNPQR", want)
	for _, typed := range []string{"abcd-efgh-jklm-npqr", "ABCDEFGHJKLMNPQR", " abcd efgh jklm npqr ", "ABCD-EFGH JKLM-NPQR"} {
		got, ok := normalizeRecoveryCode(typed)
		assert.True(t, ok, typed)
		assert.Equal(t, want, got, typed)
	}
	for _, typed := range []string{"", "ABCD-EFGH-JKLM", "ABCD-EFGH-JKLM-NPQ0", "ABCD-EFGH-JKLM-NPQRS", "ابجد-ابجد-ابجد-ابجد",
		"ABCD-EFGH-JKLM-NPQR-------------------------------------------------------"} {
		_, ok := normalizeRecoveryCode(typed)
		assert.False(t, ok, typed)
	}
}

func TestHashRecoveryCode_BelongsToItsUser(t *testing.T) {
	const user, other = "0b5f2b2e-5a0c-4c53-9c50-0d2f3a9f2a11", "7a3e0f1c-1111-4222-8333-444455556666"
	hash := hashRecoveryCode(user, "ABCDEFGHJKLMNPQR")
	assert.Len(t, hash, 64)
	assert.NotContains(t, hash, "ABCD")
	assert.Equal(t, hash, hashRecoveryCode("0B5F2B2E-5A0C-4C53-9C50-0D2F3A9F2A11", "ABCDEFGHJKLMNPQR"))
	assert.NotEqual(t, hash, hashRecoveryCode(other, "ABCDEFGHJKLMNPQR"))
	assert.NotEqual(t, hash, hashRecoveryCode(user, "ABCDEFGHJKLMNPQS"))
}
