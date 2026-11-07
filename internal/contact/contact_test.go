package contact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDigits(t *testing.T) {
	assert.Equal(t, "0123456789", Digits("۰۱۲۳۴۵۶۷۸۹"), "Persian")
	assert.Equal(t, "0123456789", Digits("٠١٢٣٤٥٦٧٨٩"), "Arabic-Indic")
	assert.Equal(t, "09123456789", Digits("۰۹١٢3456۷٨۹"), "mixed")
	assert.Equal(t, "abc@x.com", Digits("abc@x.com"))
}

func TestNormalize_MobileNumbers(t *testing.T) {
	for _, input := range []string{
		"09123456789",
		"۰۹۱۲۳۴۵۶۷۸۹",
		"٠٩١٢٣٤٥٦٧٨٩",
		"+989123456789",
		"+۹۸۹۱۲۳۴۵۶۷۸۹",
		"989123456789",
		"00989123456789",
		"9123456789",
		"0912 345 6789",
		"+98 912 345 6789",
		"0912-345-6789",
		"(0912) 345 6789",
		"  09123456789\t",
		"\u200c09123456789\u200f",
		"0912\u00a0345\u00a06789",
	} {
		assert.Equal(t, "09123456789", Normalize(input), "%q", input)
	}
}

func TestNormalize_LeavesOtherInputAlone(t *testing.T) {
	for input, want := range map[string]string{
		" Alice@Example.com ": "Alice@Example.com",
		"alice_123":           "alice_123",
		"08123456789":         "08123456789",
		"0912345678":          "0912345678",
		"091234567890":        "091234567890",
		"98912345678":         "98912345678",
		"+1 202 555 0100":     "+1 202 555 0100",
		"0912+3456789":        "0912+3456789",
		"":                    "",
	} {
		assert.Equal(t, want, Normalize(input), "%q", input)
	}
}

func TestCode(t *testing.T) {
	for input, want := range map[string]struct {
		code string
		ok   bool
	}{
		"123456":    {"123456", true},
		"۱۲۳۴۵۶":    {"123456", true},
		"١٢٣٤٥٦":    {"123456", true},
		" 123456\n": {"123456", true},
		"12345":     {"12345", false},
		"1234567":   {"1234567", false},
		"12345a":    {"12345a", false},
		"123 456":   {"123 456", false},
		"":          {"", false},
		"１２３４５６":    {"１２３４５６", false},
	} {
		code, ok := Code(input, 6)
		assert.Equal(t, want.code, code, "%q", input)
		assert.Equal(t, want.ok, ok, "%q", input)
	}
}
