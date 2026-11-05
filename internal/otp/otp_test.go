package otp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCode_ReturnsRequestedDigits(t *testing.T) {
	for _, length := range []int{1, 6, 32} {
		t.Run(fmt.Sprintf("length_%d", length), func(t *testing.T) {
			code, err := GenerateCode(length)

			require.NoError(t, err)
			assert.Len(t, code, length)
			assert.Empty(t, strings.Trim(code, "0123456789"), "only digits are allowed")
		})
	}
}

func TestGenerateCode_RejectsInvalidLengths(t *testing.T) {
	for _, length := range []int{0, -1} {
		code, err := GenerateCode(length)
		assert.Error(t, err)
		assert.Empty(t, code)
	}
}

// TestGenerateCode_DigitsAreUniform runs a chi-squared test over the digits of
// 100,000 codes. A modulo bias of 256 % 10 gives a statistic of about 220,
// while uniform digits exceed the threshold with a probability of 1 in 10^9,
// so the test catches a bias and almost never fails by chance.
func TestGenerateCode_DigitsAreUniform(t *testing.T) {
	const codes, length = 100_000, 6
	var counts [10]int
	for i := 0; i < codes; i++ {
		code, err := GenerateCode(length)
		require.NoError(t, err)
		for _, digit := range code {
			counts[digit-'0']++
		}
	}

	expected := float64(codes*length) / 10
	chiSquared := 0.0
	for _, count := range counts {
		diff := float64(count) - expected
		chiSquared += diff * diff / expected
	}
	// The critical value of the chi-squared distribution with 9 degrees of
	// freedom at p = 10^-9. At p = 0.0001 (33.72), CI runs failed by chance.
	const critical = 60.66
	assert.Less(t, chiSquared, critical, "digit counts %v are not uniform", counts)
}

func TestSettings_Validate(t *testing.T) {
	secret := strings.Repeat("s", MinSecretLength)
	valid := Settings{Secret: secret, CodeLength: 6, TTL: time.Minute, AttemptsPerCode: 3,
		ContactRequestsPerHour: 5, ContactAttemptsPerHour: 10}
	require.NoError(t, valid.Validate(true))
	assert.NoError(t, Settings{}.Validate(false), "defaults are valid, with a random secret outside release mode")

	cases := map[string]func(*Settings){
		"short code":                func(s *Settings) { s.CodeLength = 3 },
		"long code":                 func(s *Settings) { s.CodeLength = 11 },
		"negative TTL":              func(s *Settings) { s.TTL = -time.Second },
		"negative attempts":         func(s *Settings) { s.AttemptsPerCode = -1 },
		"negative contact requests": func(s *Settings) { s.ContactRequestsPerHour = -1 },
		"contact budget below code": func(s *Settings) { s.ContactAttemptsPerHour = 2 },
		"secret shorter than 32 B":  func(s *Settings) { s.Secret = "too-short" },
		"no secret in release mode": func(s *Settings) { s.Secret = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			assert.Error(t, s.Validate(true))
		})
	}
	assert.ErrorIs(t, Settings{}.Validate(true), ErrSecretRequired)
}

func TestSettingsFromConfig(t *testing.T) {
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	config.Config.OTP.Auth.Secret = "configured-secret"
	config.Config.OTP.Auth.Length = 8
	config.Config.OTP.Auth.TTL = 90
	config.Config.OTP.Auth.RetryLimit = 4
	config.Config.OTP.Auth.ContactRequestsPerHour = 6
	config.Config.OTP.Auth.ContactAttemptsPerHour = 12

	assert.Equal(t, Settings{
		Secret:                 "configured-secret",
		CodeLength:             8,
		TTL:                    90 * time.Second,
		AttemptsPerCode:        4,
		ContactRequestsPerHour: 6,
		ContactAttemptsPerHour: 12,
	}, SettingsFromConfig())
}
