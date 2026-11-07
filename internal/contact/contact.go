// Package contact normalizes what users type as a sign-in contact or a
// one-time code (#197).
//
// Many users type with a Persian keyboard, which produces Persian digits
// (U+06F0 to U+06F9), or with an Arabic one, which produces Arabic-Indic
// digits (U+0660 to U+0669). Mobile numbers also come in international
// forms, such as +989123456789, and with spaces or dashes from autofill and
// paste. All of these must reach the same account, and a code typed in
// Persian digits must be the same code.
package contact

import (
	"regexp"
	"strings"
	"unicode"
)

// Digits replaces Persian and Arabic-Indic digits with ASCII digits and
// leaves every other character as it is.
func Digits(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '۰' && r <= '۹':
			return '0' + (r - '۰')
		case r >= '٠' && r <= '٩':
			return '0' + (r - '٠')
		}
		return r
	}, s)
}

// trim removes surrounding white space, and the invisible format characters
// that keyboards and copied text add, such as the zero-width non-joiner
// (U+200C) of Persian text and directional marks.
func trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	})
}

// Normalize returns the canonical form of a contact that a user typed:
//
//   - surrounding white space and invisible characters are removed;
//   - Persian and Arabic-Indic digits become ASCII digits;
//   - an Iranian mobile number in any usual form (+98 912 345 6789,
//     0098-912-345-6789, 989123456789, or 9123456789) becomes the national
//     form 09123456789, which user_contacts stores;
//   - anything else, such as an email address, is returned trimmed and
//     otherwise unchanged. Email addresses are compared case-insensitively
//     where they are looked up, so their case is kept.
func Normalize(input string) string {
	s := Digits(trim(input))
	if mobile, ok := iranianMobile(s); ok {
		return mobile
	}
	return s
}

// iranianMobile returns the national form of s when s is an Iranian mobile
// number, possibly with separators and an international prefix.
func iranianMobile(s string) (string, bool) {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == ' ':
		default:
			return "", false
		}
	}
	digits := b.String()
	switch {
	case strings.HasPrefix(digits, "0098"):
		digits = "0" + digits[4:]
	case strings.HasPrefix(digits, "98") && len(digits) == 12:
		digits = "0" + digits[2:]
	case strings.HasPrefix(digits, "9") && len(digits) == 10:
		digits = "0" + digits
	}
	if len(digits) != 11 || !strings.HasPrefix(digits, "09") {
		return "", false
	}
	return digits, true
}

// Code returns a one-time code as typed, with surrounding white space and
// invisible characters removed and Persian and Arabic-Indic digits made
// ASCII. It reports whether the result is length ASCII digits, so that a
// malformed code can be refused without spending an attempt.
func Code(input string, length int) (string, bool) {
	code := Digits(trim(input))
	if len(code) != length {
		return code, false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return code, false
		}
	}
	return code, true
}

var (
	emailPattern  = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	mobilePattern = regexp.MustCompile(`^09\d{9}$`)
)

// Identify returns the type of a normalized contact, "email" or "mobile",
// and a masked form that is safe to log and show, or "" for anything else.
func Identify(input string) (contactType, masked string) {
	mask := func(visibleStart, visibleEnd int, str string) string {
		if len(str) <= visibleStart+visibleEnd {
			return str
		}
		return str[:visibleStart] + strings.Repeat("*", 5) + str[len(str)-visibleEnd:]
	}
	if emailPattern.MatchString(input) {
		if at := strings.Index(input, "@"); at > 1 {
			return "email", mask(3, len(input)-at, input)
		}
	}
	if mobilePattern.MatchString(input) {
		return "mobile", mask(3, 3, input)
	}
	return "", ""
}
