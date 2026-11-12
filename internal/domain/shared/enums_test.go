package shared

import (
	"errors"
	"testing"
)

func TestDecimal_IsZero(t *testing.T) {
	d := NewDecimalFromInt64(0)
	if !d.IsZero() {
		t.Errorf("expected zero decimal to be zero")
	}

	d = NewDecimalFromInt64(1)
	if d.IsZero() {
		t.Errorf("expected non-zero decimal to not be zero")
	}
}

func TestDecimal_IsNegative(t *testing.T) {
	d := NewDecimalFromInt64(-1)
	if !d.IsNegative() {
		t.Errorf("expected negative decimal to be negative")
	}

	d = NewDecimalFromInt64(1)
	if d.IsNegative() {
		t.Errorf("expected positive decimal to not be negative")
	}
}

func TestDecimal_IsPositive(t *testing.T) {
	d := NewDecimalFromInt64(1)
	if !d.IsPositive() {
		t.Errorf("expected positive decimal to be positive")
	}

	d = NewDecimalFromInt64(-1)
	if d.IsPositive() {
		t.Errorf("expected negative decimal to not be positive")
	}
}

func TestDecimal_Sign(t *testing.T) {
	d := NewDecimalFromInt64(-1)
	if d.Sign() != -1 {
		t.Errorf("expected sign -1, got %d", d.Sign())
	}

	d = NewDecimalFromInt64(0)
	if d.Sign() != 0 {
		t.Errorf("expected sign 0, got %d", d.Sign())
	}

	d = NewDecimalFromInt64(1)
	if d.Sign() != 1 {
		t.Errorf("expected sign 1, got %d", d.Sign())
	}
}

func TestDecimal_Cmp(t *testing.T) {
	d1 := NewDecimalFromInt64(1)
	d2 := NewDecimalFromInt64(2)

	if d1.Cmp(d2) >= 0 {
		t.Errorf("expected 1 < 2")
	}

	if d2.Cmp(d1) <= 0 {
		t.Errorf("expected 2 > 1")
	}

	d3 := NewDecimalFromInt64(1)
	if d1.Cmp(d3) != 0 {
		t.Errorf("expected 1 == 1")
	}
}

func TestDecimal_Equal(t *testing.T) {
	d1 := NewDecimalFromInt64(1)
	d2 := NewDecimalFromInt64(1)
	d3 := NewDecimalFromInt64(2)

	if !d1.Equal(d2) {
		t.Errorf("expected 1 == 1")
	}

	if d1.Equal(d3) {
		t.Errorf("expected 1 != 2")
	}
}

func TestDecimal_LessThan(t *testing.T) {
	d1 := NewDecimalFromInt64(1)
	d2 := NewDecimalFromInt64(2)

	if !d1.LessThan(d2) {
		t.Errorf("expected 1 < 2")
	}

	if d2.LessThan(d1) {
		t.Errorf("expected not 2 < 1")
	}
}

func TestDecimal_GreaterThan(t *testing.T) {
	d1 := NewDecimalFromInt64(2)
	d2 := NewDecimalFromInt64(1)

	if !d1.GreaterThan(d2) {
		t.Errorf("expected 2 > 1")
	}

	if d2.GreaterThan(d1) {
		t.Errorf("expected not 1 > 2")
	}
}

func TestDecimal_Add(t *testing.T) {
	d1 := NewDecimalFromInt64(1)
	d2 := NewDecimalFromInt64(2)
	result := d1.Add(d2)

	expected := NewDecimalFromInt64(3)
	if !result.Equal(expected) {
		t.Errorf("expected 1 + 2 = 3, got %s", result.String())
	}
}

func TestDecimal_Sub(t *testing.T) {
	d1 := NewDecimalFromInt64(5)
	d2 := NewDecimalFromInt64(3)
	result := d1.Sub(d2)

	expected := NewDecimalFromInt64(2)
	if !result.Equal(expected) {
		t.Errorf("expected 5 - 3 = 2, got %s", result.String())
	}
}

func TestDecimal_Mul(t *testing.T) {
	d1 := NewDecimalFromInt64(3)
	d2 := NewDecimalFromInt64(4)
	result := d1.Mul(d2)

	expected := NewDecimalFromInt64(12)
	if !result.Equal(expected) {
		t.Errorf("expected 3 * 4 = 12, got %s", result.String())
	}
}

func TestDecimal_Div(t *testing.T) {
	result, err := NewDecimalFromInt64(10).Div(NewDecimalFromInt64(4))
	if err != nil || !result.Equal(MustNewDecimalFromString("2.5")) {
		t.Errorf("expected 10 / 4 = 2.5, got %s, %v", result.String(), err)
	}
	for _, zero := range []Decimal{{}, NewDecimalFromInt64(0), MustNewDecimalFromString("0.000")} {
		if _, err := NewDecimalFromInt64(1).Div(zero); !errors.Is(err, ErrDivisionByZero) {
			t.Errorf("1 / %s: expected ErrDivisionByZero, got %v", zero.String(), err)
		}
	}
}

func TestDecimal_QuoRem(t *testing.T) {
	q, r, err := NewDecimalFromInt64(10).QuoRem(NewDecimalFromInt64(3))
	if err != nil || !q.Equal(NewDecimalFromInt64(3)) || !r.Equal(NewDecimalFromInt64(1)) {
		t.Errorf("expected 10 = 3*3 + 1, got %s, %s, %v", q.String(), r.String(), err)
	}
	if _, _, err := NewDecimalFromInt64(10).QuoRem(Decimal{}); !errors.Is(err, ErrDivisionByZero) {
		t.Errorf("expected ErrDivisionByZero, got %v", err)
	}
}

func TestDecimal_Pow(t *testing.T) {
	for _, tc := range []struct{ base, exp, want string }{
		{"2", "3", "8"},
		{"10", "-2", "0.01"},
		{"4", "0.5", "2"},
		{"-2", "3", "-8"},
	} {
		got, err := MustNewDecimalFromString(tc.base).Pow(MustNewDecimalFromString(tc.exp))
		if err != nil || !got.Equal(MustNewDecimalFromString(tc.want)) {
			t.Errorf("%s ^ %s: expected %s, got %s, %v", tc.base, tc.exp, tc.want, got.String(), err)
		}
	}
	for _, tc := range []struct{ base, exp string }{{"0", "0"}, {"0", "-1"}, {"-4", "0.5"}} {
		if _, err := MustNewDecimalFromString(tc.base).Pow(MustNewDecimalFromString(tc.exp)); err == nil {
			t.Errorf("%s ^ %s is undefined and must fail instead of returning 0", tc.base, tc.exp)
		}
	}
}

func TestDecimal_UnmarshalJSON(t *testing.T) {
	for input, want := range map[string]string{
		`"1.5"`:                                 "1.5",
		`1.5`:                                   "1.5",
		`-0.000000000000000001`:                 "-0.000000000000000001",
		`123456789012345678.123456789012345678`: "123456789012345678.123456789012345678",
		`1e3`:                                   "1000",
		`"0"`:                                   "0",
	} {
		var d Decimal
		if err := d.UnmarshalJSON([]byte(input)); err != nil || d.String() != want {
			t.Errorf("%s: expected %s, got %s, %v", input, want, d.String(), err)
		}
	}
	for _, input := range []string{`null`, `true`, `"abc"`, `""`, `[1]`, `{"a":1}`, `1.2.3`, ``} {
		var d Decimal
		if err := d.UnmarshalJSON([]byte(input)); err == nil {
			t.Errorf("%s: expected an error, got %s", input, d.String())
		}
	}
}

func TestDecimal_Abs(t *testing.T) {
	d := NewDecimalFromInt64(-5)
	result := d.Abs()

	expected := NewDecimalFromInt64(5)
	if !result.Equal(expected) {
		t.Errorf("expected |-5| = 5, got %s", result.String())
	}
}

func TestDecimal_Neg(t *testing.T) {
	d := NewDecimalFromInt64(5)
	result := d.Neg()

	expected := NewDecimalFromInt64(-5)
	if !result.Equal(expected) {
		t.Errorf("expected -5, got %s", result.String())
	}
}

func TestDecimal_Round(t *testing.T) {
	d := NewDecimalFromInt64(123456)
	result := d.Round(-2)

	expected := NewDecimalFromInt64(123500)
	if !result.Equal(expected) {
		t.Errorf("expected 123500, got %s", result.String())
	}
}

func TestDecimal_Ceil(t *testing.T) {
	d, _ := NewDecimalFromString("3.14")
	result := d.Ceil()

	expected := NewDecimalFromInt64(4)
	if !result.Equal(expected) {
		t.Errorf("expected ceil(3.14) = 4, got %s", result.String())
	}
}

func TestDecimal_Floor(t *testing.T) {
	d, _ := NewDecimalFromString("3.14")
	result := d.Floor()

	expected := NewDecimalFromInt64(3)
	if !result.Equal(expected) {
		t.Errorf("expected floor(3.14) = 3, got %s", result.String())
	}
}

func TestDecimal_InexactFloat64(t *testing.T) {
	d := NewDecimalFromInt64(123)
	f := d.InexactFloat64()

	if f != 123.0 {
		t.Errorf("expected 123.0, got %f", f)
	}
}

func TestNewDecimalFromString(t *testing.T) {
	d, err := NewDecimalFromString("123.456")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	expected, _ := NewDecimalFromString("123.456")
	if !d.Equal(expected) {
		t.Errorf("expected 123.456, got %s", d.String())
	}
}

func TestNewDecimalFromString_Invalid(t *testing.T) {
	_, err := NewDecimalFromString("invalid")
	if err == nil {
		t.Errorf("expected error for invalid decimal string")
	}
}

func TestDecimal_MarshalJSON(t *testing.T) {
	d := NewDecimalFromInt64(123)
	data, err := d.MarshalJSON()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// MarshalJSON returns quoted string (e.g., "123")
	if string(data) != "\"123\"" {
		t.Errorf("expected JSON '\"123\"', got %s", string(data))
	}
}

func TestAssetType_String(t *testing.T) {
	for want, v := range map[string]AssetType{
		"FIAT": AssetTypeFiat, "COIN": AssetTypeCoin, "TOKEN": AssetTypeToken, "STABLE": AssetTypeStable, "WRAPPED": AssetTypeWrapped,
	} {
		if v.String() != want {
			t.Errorf("%q.String() = %q", want, v.String())
		}
	}
}

func TestAssetStatus_String(t *testing.T) {
	for want, v := range map[string]AssetStatus{
		"ACTIVE": AssetStatusActive, "INACTIVE": AssetStatusInactive, "DEPRECATED": AssetStatusDeprecated, "DELISTED": AssetStatusDelisted,
	} {
		if v.String() != want {
			t.Errorf("%q.String() = %q", want, v.String())
		}
	}
}
