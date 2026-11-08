package source

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Decimal reads an amount that a source sent as a JSON number or string,
// without passing it through a float, so that every digit stays. It
// returns nil for null, an empty string, or a value that is not a number,
// so that a missing figure stays missing.
func Decimal(v any) *shared.Decimal {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	default:
		return nil
	}
	if s == "" {
		return nil
	}
	d, err := shared.NewDecimalFromString(s)
	if err != nil {
		return nil
	}
	return &d
}

// RequiredDecimal reads an amount that must be there.
func RequiredDecimal(field string, v any) (shared.Decimal, error) {
	d := Decimal(v)
	if d == nil {
		return shared.Decimal{}, fmt.Errorf("source: %s is not a number: %v", field, v)
	}
	return *d, nil
}

// UnixMillis reads a time in milliseconds since 1970, as a JSON number or
// string, in UTC.
func UnixMillis(v any) (time.Time, error) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = x
	default:
		return time.Time{}, fmt.Errorf("source: %v is not a time", v)
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, fmt.Errorf("source: %q is not a time in milliseconds", s)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// Int reads a whole number, and nil when there is none or it is not
// positive, such as a missing rank.
func Int(v any) *int {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	i, err := strconv.Atoi(n.String())
	if err != nil || i <= 0 {
		return nil
	}
	return &i
}
