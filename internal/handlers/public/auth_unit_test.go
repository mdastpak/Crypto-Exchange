package public

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentifyInputType(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantType   string
		wantMasked string
	}{
		{"email", "alice@example.com", "email", "ali*****@example.com"},
		{"mobile", "09123456789", "mobile", "091*****789"},
		{"username", "alice_123", "username", "al*****123"},
		{"short username", "abc", "", ""},
		{"numeric username", "123456", "", ""},
		{"leading underscore", "_alice1", "", ""},
		{"double underscore", "alice__1", "", ""},
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
