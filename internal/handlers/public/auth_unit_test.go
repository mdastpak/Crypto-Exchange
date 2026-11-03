package public

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateOTP_ReturnsRequestedDigits(t *testing.T) {
	for _, length := range []int{1, 6, 32} {
		t.Run(fmt.Sprintf("length_%d", length), func(t *testing.T) {
			otp := GenerateOTP(length)

			assert.Len(t, otp, length)
			for _, digit := range otp {
				assert.Contains(t, "0123456789", string(digit))
			}
		})
	}
}

func TestGenerateOTP_ProducesDifferentValues(t *testing.T) {
	values := make(map[string]struct{})
	for i := 0; i < 20; i++ {
		values[GenerateOTP(12)] = struct{}{}
	}

	assert.Greater(t, len(values), 1)
}

func TestIdentifyInputType(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantType   string
		wantMasked string
	}{
		{"email", "alice@example.com", "email", "ali*****@example.com"},
		{"mobile", "09123456789", "mobile", "091*****789"},
		{"username", "alice_123", "", ""},
		{"short username", "abc", "", ""},
		{"numeric username", "123456", "", ""},
		{"invalid email", "alice@", "", ""},
		{"invalid mobile", "08123456789", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotMasked := IdentifyInputType(tt.input)
			assert.Equal(t, tt.wantType, gotType)
			assert.Equal(t, tt.wantMasked, gotMasked)
			if gotMasked != "" {
				assert.True(t, strings.Contains(gotMasked, "*****"))
			}
		})
	}
}

func TestValidateUUID(t *testing.T) {
	assert.True(t, validateUUID("550e8400-e29b-41d4-a716-446655440000"))
	assert.False(t, validateUUID(""))
	assert.False(t, validateUUID("not-a-uuid"))
}
