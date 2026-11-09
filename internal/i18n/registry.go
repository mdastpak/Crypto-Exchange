package i18n

import (
	"fmt"
	"strings"

	"golang.org/x/text/language"
)

// Registry is the set of supported locales and their translations. It does
// not change after New, so it is safe for concurrent use.
type Registry struct {
	locales []Locale
	byCode  map[string]int
	def     int
	matcher language.Matcher
	tags    []language.Tag
}

// New builds the registry of codes, in the order a language switcher lists
// them, with defaultCode as the fallback locale. Every code must be a valid
// BCP 47 tag, listed once; English and defaultCode must be among them.
func New(defaultCode string, codes []string) (*Registry, error) {
	r := &Registry{byCode: map[string]int{}}
	hasEnglish := false
	for _, code := range codes {
		tag, err := parseTag(code)
		if err != nil {
			return nil, fmt.Errorf("locale %s", err)
		}
		l := newLocale(tag)
		if _, dup := r.byCode[l.Code]; dup {
			return nil, fmt.Errorf("locale %q is listed twice", l.Code)
		}
		r.byCode[l.Code] = len(r.locales)
		r.locales = append(r.locales, l)
		r.tags = append(r.tags, tag)
		if l.Code == English {
			hasEnglish = true
		}
	}
	if !hasEnglish {
		return nil, fmt.Errorf("the locales must include %q, the language of every message that has no translation", English)
	}
	def, err := ParseCode(defaultCode)
	if err != nil {
		return nil, fmt.Errorf("default locale %s", err)
	}
	index, ok := r.byCode[def]
	if !ok {
		return nil, fmt.Errorf("default locale %q is not one of the locales (%s)", def, strings.Join(r.Codes(), ", "))
	}
	r.def = index
	r.matcher = language.NewMatcher(r.tags)
	return r, nil
}

// MustNew is New for tests and fixed lists; it panics on an error.
func MustNew(defaultCode string, codes ...string) *Registry {
	r, err := New(defaultCode, codes)
	if err != nil {
		panic(err)
	}
	return r
}

// Locales returns the supported locales, in their configured order.
func (r *Registry) Locales() []Locale {
	return append([]Locale(nil), r.locales...)
}

// Codes returns the codes of the supported locales.
func (r *Registry) Codes() []string {
	codes := make([]string, len(r.locales))
	for i, l := range r.locales {
		codes[i] = l.Code
	}
	return codes
}

// Default returns the fallback locale.
func (r *Registry) Default() Locale {
	return r.locales[r.def]
}

// Lookup returns the supported locale that code names: the locale of that
// exact tag, or else the first locale of the same language, so that
// "fa-IR" finds "fa" and "en" finds "en-GB" when only that is supported.
// An invalid or unsupported code reports false.
func (r *Registry) Lookup(code string) (Locale, bool) {
	tag, err := parseTag(code)
	if err != nil {
		return Locale{}, false
	}
	if i, ok := r.byCode[tag.String()]; ok {
		return r.locales[i], true
	}
	base, _ := tag.Base()
	for _, l := range r.locales {
		if l.Base() == base.String() {
			return l, true
		}
	}
	return Locale{}, false
}

// Match returns the supported locale that an Accept-Language header asks
// for, or false when it asks for none of them closely enough. A language
// close to a supported one, such as "en-GB" for "en", matches; a merely
// related one does not, so a visitor who asks for Turkish is not shown
// Azerbaijani.
func (r *Registry) Match(acceptLanguage string) (Locale, bool) {
	if strings.TrimSpace(acceptLanguage) == "" {
		return Locale{}, false
	}
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		return Locale{}, false
	}
	_, index, confidence := r.matcher.Match(tags...)
	if confidence < language.High {
		return Locale{}, false
	}
	return r.locales[index], true
}

// Translate returns message in locale l: the translation of the message
// itself, or else of code, or else message unchanged. A code is only used
// when it is specific to one kind of error, since the generic code of a
// status covers messages of different meanings; the caller passes "" for
// those.
func (r *Registry) Translate(l Locale, code, message string) (string, bool) {
	if l.Base() == English {
		return message, false
	}
	c := catalogFor(l)
	if c == nil {
		return message, false
	}
	if t, ok := c.Messages[message]; ok {
		return t, true
	}
	if code != "" {
		if t, ok := c.Codes[code]; ok {
			return t, true
		}
	}
	return message, false
}
