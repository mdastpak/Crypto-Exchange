package userauth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		password string
		want     error
	}{
		"a passphrase":            {"correct horse battery", nil},
		"exactly the least":       {"abcdefghij", nil},
		"Persian text":            {"گذرواژه\u200cی خیلی خوب", nil},
		"one short":               {"abcdefghi", ErrPasswordTooShort},
		"empty":                   {"", ErrPasswordTooShort},
		"one character repeated":  {strings.Repeat("a", 20), ErrPasswordTooSimple},
		"four characters":         {"abcdabcdabcd", ErrPasswordTooSimple},
		"over the most bytes":     {strings.Repeat("ab cd ef", 40), ErrPasswordTooLong},
		"invalid UTF-8":           {"abcdefghij\xff\xfe", ErrPasswordTooLong},
		"a NUL, which is refused": {"abcdefghij\x00k", ErrPasswordTooLong},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckPolicy(tc.password, 10)
			if tc.want == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.want)
		})
	}
	// The length counts characters, not bytes.
	assert.NoError(t, CheckPolicy("گذرواژه خوب", 10))
}
