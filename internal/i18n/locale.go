// Package i18n holds the locales that the product supports, their text
// direction, the translations of the API's messages, and the negotiation of
// the locale of each request (#283).
//
// The package depends on no other package of the module, so that the
// configuration can validate its settings with it.
package i18n

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// Text directions, as the HTML dir attribute names them.
const (
	LTR = "ltr"
	RTL = "rtl"
)

// English is the language of every message the server writes, and so the
// fallback of every translation. Every registry supports it.
const English = "en"

// Locale is a supported locale.
type Locale struct {
	// Code is the canonical BCP 47 tag, such as "fa" or "en-GB".
	Code string `json:"code" example:"fa"`
	// Name is the name of the language in English.
	Name string `json:"name" example:"Persian"`
	// NativeName is the name of the language in itself, for a language
	// switcher.
	NativeName string `json:"native_name" example:"فارسی"`
	// Dir is the direction of its text: "ltr" or "rtl".
	Dir string `json:"dir" example:"rtl" enums:"ltr,rtl"`

	tag language.Tag
}

// Base returns the language of the locale without its region or script,
// such as "fa" for "fa-AF".
func (l Locale) Base() string {
	base, _ := l.tag.Base()
	return base.String()
}

// rtlScripts are the scripts written from right to left that a locale's
// language is likely written in.
var rtlScripts = map[string]bool{
	"Arab": true, "Hebr": true, "Thaa": true, "Syrc": true, "Nkoo": true,
	"Adlm": true, "Rohg": true, "Mand": true, "Samr": true, "Mend": true,
}

// direction returns the direction of the script that tag is likely written
// in: Persian and Arabic are RTL, Azerbaijani in Latin script is LTR, and
// "az-Arab" is RTL.
func direction(tag language.Tag) string {
	script, _ := tag.Script()
	if rtlScripts[script.String()] {
		return RTL
	}
	return LTR
}

func newLocale(tag language.Tag) Locale {
	l := Locale{Code: tag.String(), Dir: direction(tag), tag: tag}
	l.Name = display.English.Tags().Name(tag)
	l.NativeName = display.Self.Name(tag)
	if l.Name == "" {
		l.Name = l.Code
	}
	if l.NativeName == "" {
		l.NativeName = l.Name
	}
	return l
}

// ParseCode returns the canonical form of a BCP 47 tag. An underscore is
// read as a hyphen, so "fa_IR" is "fa-IR".
func ParseCode(code string) (string, error) {
	tag, err := parseTag(code)
	if err != nil {
		return "", err
	}
	return tag.String(), nil
}

func parseTag(code string) (language.Tag, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), "_", "-")
	if code == "" {
		return language.Und, errors.New("is empty")
	}
	tag, err := language.Parse(code)
	if err != nil {
		return language.Und, fmt.Errorf("%q is not a BCP 47 language tag", code)
	}
	if tag == language.Und {
		return language.Und, fmt.Errorf("%q names no language", code)
	}
	return tag, nil
}
