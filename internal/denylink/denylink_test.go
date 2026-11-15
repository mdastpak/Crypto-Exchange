package denylink

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewToken_IsBase62OfItsLength: tokens are 12 characters of Base62,
// all different, and every character appears.
func TestNewToken_IsBase62OfItsLength(t *testing.T) {
	seen := map[string]bool{}
	chars := map[rune]bool{}
	for range 2000 {
		token, err := newToken()
		require.NoError(t, err)
		require.True(t, Valid(token), token)
		require.False(t, seen[token], "a token repeats")
		seen[token] = true
		for _, r := range token {
			chars[r] = true
		}
	}
	assert.Len(t, chars, len(alphabet))
}

func TestValid(t *testing.T) {
	assert.True(t, Valid("Ab3xK9q2LmPq"))
	for _, token := range []string{"", "Ab3xK9q2LmP", "Ab3xK9q2LmPqr", "Ab3xK9q2LmP-", "Ab3xK9q2LmP ", "Ab3xK9q2Lm/q", "Ab3xK9q2LmP\x00"} {
		assert.False(t, Valid(token), "%q", token)
	}
}

func TestHash_IsTheSHA256OfTheToken(t *testing.T) {
	assert.Len(t, hash("Ab3xK9q2LmPq"), 32)
	assert.Equal(t, hash("Ab3xK9q2LmPq"), hash("Ab3xK9q2LmPq"))
	assert.NotEqual(t, hash("Ab3xK9q2LmPq"), hash("Ab3xK9q2LmPr"))
}
