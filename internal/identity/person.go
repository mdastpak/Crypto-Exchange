package identity

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Person is who a check is about: a national ID and the date of birth that
// goes with it (#158). The providers check a card, an IBAN, or the national
// ID itself against both.
type Person struct {
	NationalID string
	// BirthDate is the date of birth, in the Gregorian calendar, at
	// midnight UTC.
	BirthDate time.Time
}

// IdentityInquiry checks that a national ID exists and was born on a date
// (#158), at the national registry through a KYC provider. Implementations
// are safe for concurrent use.
type IdentityInquiry interface {
	// Provider names the provider, as the checks record it.
	Provider() string
	// Identify reports whether p.NationalID was born on p.BirthDate. An
	// error means that the provider could not answer; it wraps
	// ErrUnavailable when trying again later may work.
	Identify(ctx context.Context, p Person) (Match, error)
}

// ErrUnsupported is a check that the provider does not offer, such as the
// owner of a bank account number at Jibit, which checks cards and IBANs.
// Trying again does not help.
var ErrUnsupported = errors.New("the identity provider does not offer this check")

// ParseBirthDate reads a date of birth in the form YYYY-MM-DD of the
// Gregorian calendar. It refuses a date in the future, and one more than
// 130 years ago.
func ParseBirthDate(s string, now time.Time) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, errors.New("not a date in the form YYYY-MM-DD")
	}
	if d.After(now) || d.Before(now.AddDate(-130, 0, 0)) {
		return time.Time{}, fmt.Errorf("not a date of birth")
	}
	return d, nil
}

// jalali returns the date of the Persian (Solar Hijri) calendar of a
// Gregorian date, in the form YYYYMMDD that Jibit takes: the conversion of
// jalaali.js and of the jdf library, exact for every year that a date of
// birth can have.
func jalali(t time.Time) string {
	gy, gm, gd := t.Date()
	monthDays := [...]int{0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334}
	gy2 := gy
	if gm > 2 {
		gy2 = gy + 1
	}
	days := 355666 + 365*gy + (gy2+3)/4 - (gy2+99)/100 + (gy2+399)/400 + gd + monthDays[gm-1]
	jy := -1595 + 33*(days/12053)
	days %= 12053
	jy += 4 * (days / 1461)
	days %= 1461
	if days > 365 {
		jy += (days - 1) / 365
		days = (days - 1) % 365
	}
	var jm, jd int
	if days < 186 {
		jm, jd = 1+days/31, 1+days%31
	} else {
		jm, jd = 7+(days-186)/30, 1+(days-186)%30
	}
	return fmt.Sprintf("%04d%02d%02d", jy, jm, jd)
}
